package server

import (
	"bufio"
	"bytes"
	"context"
	"html/template"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"emperror.dev/errors"
	"github.com/apex/log"
	"github.com/docker/docker/client"
	"k8s.io/client-go/kubernetes"

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/environment"
	"github.com/pterodactyl/wings/remote"
	"github.com/pterodactyl/wings/system"

	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
)

// Install executes the installation stack for a server process. Bubbles any
// errors up to the calling function which should handle contacting the panel to
// notify it of the server state.
//
// Pass true as the first argument in order to execute a server sync before the
// process to ensure the latest information is used.
func (s *Server) Install(sync bool) error {
	if sync {
		s.Log().Info("syncing server state with remote source before executing installation process")
		if err := s.Sync(); err != nil {
			return errors.WrapIf(err, "install: failed to sync server state with Panel")
		}
	}

	var err error
	if !s.Config().SkipEggScripts {
		// Send the start event so the Panel can automatically update. We don't send this unless the process
		// is actually going to run, otherwise all sorts of weird rapid UI behavior happens since there isn't
		// an actual install process being executed.
		s.Events().Publish(InstallStartedEvent, "")

		err = s.internalInstall()
	} else {
		s.Log().Info("server configured to skip running installation scripts for this egg, not executing process")
	}

	s.Log().WithField("was_successful", err == nil).Debug("notifying panel of server install state")
	if serr := s.SyncInstallState(err == nil); serr != nil {
		l := s.Log().WithField("was_successful", err == nil)

		// If the request was successful but there was an error with this request, attach the
		// error to this log entry. Otherwise ignore it in this log since whatever is calling
		// this function should handle the error and will end up logging the same one.
		if err == nil {
			l.WithField("error", err)
		}

		l.Warn("failed to notify panel of server install state")
	}

	// Ensure that the server is marked as offline at this point, otherwise you end up
	// with a blank value which is a bit confusing.
	s.Environment.SetState(environment.ProcessOfflineState)

	// Push an event to the websocket so we can auto-refresh the information in the panel once
	// the install is completed.
	s.Events().Publish(InstallCompletedEvent, "")

	return errors.WithStackIf(err)
}

// Reinstalls a server's software by utilizing the install script for the server egg. This
// does not touch any existing files for the server, other than what the script modifies.
func (s *Server) Reinstall() error {
	if s.Environment.State() != environment.ProcessOfflineState {
		s.Log().Debug("waiting for server instance to enter a stopped state")
		if err := s.Environment.WaitForStop(s.Context(), time.Second*10, true); err != nil {
			return errors.WrapIf(err, "install: failed to stop running environment")
		}
	}

	return s.Install(true)
}

// Internal installation function used to simplify reporting back to the Panel.
func (s *Server) internalInstall() error {
	script, err := s.client.GetInstallationScript(s.Context(), s.ID())
	if err != nil {
		return err
	}
	p, err := NewInstallationProcess(s, &script)
	if err != nil {
		return err
	}

	s.Log().Info("beginning installation process for server")
	if err := p.Run(); err != nil {
		return err
	}

	s.Log().Info("completed installation process for server")
	return nil
}

type InstallationProcess struct {
	Server    *Server
	Script    *remote.InstallationScript
	client    *client.Client
	clientset *kubernetes.Clientset
}

// Generates a new installation process struct that will be used to create containers,
// and otherwise perform installation commands for a server.
func NewInstallationProcess(s *Server, script *remote.InstallationScript) (*InstallationProcess, error) {
	proc := &InstallationProcess{
		Script: script,
		Server: s,
	}

	if c, err := environment.Docker(); err != nil {
		return nil, err
	} else {
		proc.client = c
	}

	if _, c, err := environment.Kubernetes(); err != nil {
		return nil, err
	} else {
		proc.clientset = c
	}

	return proc, nil
}

// Determines if the server is actively running the installation process by checking the status
// of the installer lock.
func (s *Server) IsInstalling() bool {
	return s.installing.Load()
}

func (s *Server) IsTransferring() bool {
	return s.transferring.Load()
}

func (s *Server) SetTransferring(state bool) {
	s.transferring.Store(state)
}

func (s *Server) IsRestoring() bool {
	return s.restoring.Load()
}

func (s *Server) SetRestoring(state bool) {
	s.restoring.Store(state)
}

// Removes the installer container for the server.
func (ip *InstallationProcess) RemoveContainer() error {
	err := ip.clientset.CoreV1().Pods(config.Get().System.Namespace).Delete(ip.Server.Context(), ip.Server.ID()+"-installer", metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	err = ip.clientset.CoreV1().ConfigMaps(config.Get().System.Namespace).Delete(ip.Server.Context(), ip.Server.ID()+"-configmap", metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// Run runs the installation process, this is done as in a background thread.
// This will configure the required environment, and then spin up the
// installation container. Once the container finishes installing the results
// are stored in an installation log in the server's configuration directory.
func (ip *InstallationProcess) Run() error {
	ip.Server.Log().Debug("acquiring installation process lock")
	if !ip.Server.installing.SwapIf(true) {
		return errors.New("install: cannot obtain installation lock")
	}

	// We now have an exclusive lock on this installation process. Ensure that whenever this
	// process is finished that the semaphore is released so that other processes and be executed
	// without encountering a wait timeout.
	defer func() {
		ip.Server.Log().Debug("releasing installation process lock")
		ip.Server.installing.Store(false)
	}()

	if err := ip.BeforeExecute(); err != nil {
		return err
	}

	cID, err := ip.Execute()
	if err != nil {
		_ = ip.RemoveContainer()
		return err
	}

	// If this step fails, log a warning but don't exit out of the process. This is completely
	// internal to the daemon's functionality, and does not affect the status of the server itself.
	if err := ip.AfterExecute(cID); err != nil {
		ip.Server.Log().WithField("error", err).Warn("failed to complete after-execute step of installation process")
	}

	return nil
}

// Returns the location of the temporary data for the installation process.
func (ip *InstallationProcess) tempDir() string {
	return filepath.Join(config.Get().System.TmpDirectory, ip.Server.ID())
}

// Writes the installation script to a temporary file on the host machine so that it
// can be properly mounted into the installation container and then executed.
func (ip *InstallationProcess) writeScriptToDisk() error {
	// Make sure the temp directory root exists before trying to make a directory within it. The
	// ioutil.TempDir call expects this base to exist, it won't create it for you.
	if err := os.MkdirAll(ip.tempDir(), 0o700); err != nil {
		return errors.WithMessage(err, "could not create temporary directory for install process")
	}

	f, err := os.OpenFile(filepath.Join(ip.tempDir(), "install.sh"), os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return errors.WithMessage(err, "failed to write server installation script to disk before mount")
	}
	defer f.Close()

	w := bufio.NewWriter(f)

	scanner := bufio.NewScanner(bytes.NewReader([]byte(ip.Script.Script)))
	for scanner.Scan() {
		w.WriteString(scanner.Text() + "\n")
	}

	if err := scanner.Err(); err != nil {
		return err
	}

	w.Flush()

	return nil
}

// BeforeExecute runs before the container is executed. This pulls down the
// required docker container image as well as writes the installation script to
// the disk. This process is executed in an async manner, if either one fails
// the error is returned.
func (ip *InstallationProcess) BeforeExecute() error {
	if err := ip.writeScriptToDisk(); err != nil {
		return errors.WithMessage(err, "failed to write installation script to disk")
	}

	var zero int64 = 0
	policy := metav1.DeletePropagationForeground

	if err := ip.clientset.CoreV1().PersistentVolumeClaims(config.Get().System.Namespace).Delete(context.Background(), ip.Server.ID()+"-pvc", metav1.DeleteOptions{GracePeriodSeconds: &zero, PropagationPolicy: &policy}); err != nil {
		if !apierrors.IsNotFound(err) {
			return errors.WithMessage(err, "failed to remove pvc before running installation")
		}
	}

	if err := ip.RemoveContainer(); err != nil {
		return errors.WithMessage(err, "failed to remove existing install container for server")
	}
	return nil
}

// Returns the log path for the installation process.
func (ip *InstallationProcess) GetLogPath() string {
	return filepath.Join(config.Get().System.LogDirectory, "/install", ip.Server.ID()+".log")
}

// Cleans up after the execution of the installation process. This grabs the logs from the
// process to store in the server configuration directory, and then destroys the associated
// installation container.
func (ip *InstallationProcess) AfterExecute(containerId string) error {
	defer ip.RemoveContainer()

	ip.Server.Log().WithField("container_id", containerId).Debug("pulling installation logs for server")
	reader := ip.clientset.CoreV1().Pods(config.Get().System.Namespace).GetLogs(ip.Server.ID()+"-installer", &corev1.PodLogOptions{
		Follow: false,
	})
	podLogs, err := reader.Stream(ip.Server.Context())
	if err != nil {
		return err
	}
	defer podLogs.Close()

	f, err := os.OpenFile(ip.GetLogPath(), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()

	// We write the contents of the container output to a more "permanent" file so that they
	// can be referenced after this container is deleted. We'll also include the environment
	// variables passed into the container to make debugging things a little easier.
	ip.Server.Log().WithField("path", ip.GetLogPath()).Debug("writing most recent installation logs to disk")

	tmpl, err := template.New("header").Parse(`Pterodactyl Server Installation Log

|
| Details
| ------------------------------
  Server UUID:          {{.Server.ID}}
  Container Image:      {{.Script.ContainerImage}}
  Container Entrypoint: {{.Script.Entrypoint}}

|
| Environment Variables
| ------------------------------
{{ range $key, $value := .Server.GetEnvironmentVariables }}  {{ $value }}
{{ end }}

|
| Script Output
| ------------------------------
`)
	if err != nil {
		return err
	}

	if err := tmpl.Execute(f, ip); err != nil {
		return err
	}

	if _, err := io.Copy(f, podLogs); err != nil {
		return err
	}

	return nil
}

// Return a condition function that indicates whether the given pod is currently running
func (ip *InstallationProcess) isPodRunning(podName, namespace string) wait.ConditionFunc {
	return func() (bool, error) {
		pod, err := ip.clientset.CoreV1().Pods(config.Get().System.Namespace).Get(context.TODO(), podName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}

		switch pod.Status.Phase {
		case v1.PodRunning:
			return true, nil
		case v1.PodFailed, v1.PodSucceeded:
			return false, nil
		}
		return false, nil
	}
}

// Return a condition function that indicates whether the given pod is completed
func (ip *InstallationProcess) isPodSucceeded(podName, namespace string) wait.ConditionFunc {
	return func() (bool, error) {
		pod, err := ip.clientset.CoreV1().Pods(config.Get().System.Namespace).Get(context.TODO(), podName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}

		switch pod.Status.Phase {
		case v1.PodSucceeded:
			return true, nil
		case v1.PodFailed:
			return false, nil
		}
		return false, nil
	}
}

// Poll up to timeout seconds for pod to enter running state.
// Returns an error if the pod never enters the running state.
func (ip *InstallationProcess) waitForPodRunning() error {
	return wait.PollInfinite(time.Second, ip.isPodRunning(ip.Server.ID()+"-installer", config.Get().System.Namespace))
}

// Poll up to timeout seconds for pod to enter running state.
// Returns an error if the pod never enters the running state.
func (ip *InstallationProcess) waitForPodSucceeded() error {
	return wait.PollInfinite(time.Second, ip.isPodSucceeded(ip.Server.ID()+"-installer", config.Get().System.Namespace))
}

func ptrstring(p string) *string {
	return &p
}

// Execute executes the installation process inside a specially created docker
// container.
func (ip *InstallationProcess) Execute() (string, error) {
	// Create a child context that is canceled once this function is done running. This
	// will also be canceled if the parent context (from the Server struct) is canceled
	// which occurs if the server is deleted.
	ctx, cancel := context.WithCancel(ip.Server.Context())
	defer cancel()

	fileContents, err := os.ReadFile(filepath.Join(ip.tempDir(), "install.sh"))
	if err != nil {
		return "", err
	}

	// Create configmap object
	configmap := &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{
			Kind:       "ConfigMap",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: ip.Server.ID() + "-configmap",
		},
		Data: map[string]string{
			"install.sh": string(fileContents),
		},
	}

	ip.Server.Log().WithField("configmap", ip.Server.ID()+"-configmap").Info("creating configmap")

	go func() {
		_, err = ip.clientset.CoreV1().ConfigMaps(config.Get().System.Namespace).Create(ctx, configmap, metav1.CreateOptions{})

		if err != nil && apierrors.IsAlreadyExists(err) {
			ip.Server.Log().WithField("error", err).Warn("failed to create configmap")
		}
	}()

	// Create pvc object
	pvc := &corev1.PersistentVolumeClaim{
		TypeMeta: metav1.TypeMeta{
			Kind:       "PersistentVolumeClaim",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: ip.Server.ID() + "-pvc",
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{
				corev1.PersistentVolumeAccessMode("ReadWriteOnce"),
			},
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					"storage": *resource.NewQuantity(ip.Server.DiskSpace(), resource.BinarySI),
				},
			},
			StorageClassName: ptrstring("manual"),
		},
	}

	ip.Server.Log().WithField("pvc", ip.Server.ID()+"-pvc").Info("creating pvc")

	_, err = ip.clientset.CoreV1().PersistentVolumeClaims(config.Get().System.Namespace).Create(context.TODO(), pvc, metav1.CreateOptions{})
	if err != nil {
		return "", err
	}

	// Create pod object
	pod := &corev1.Pod{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Pod",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: ip.Server.ID() + "-installer",
		},
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{
				{
					Name: "storage",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: ip.Server.ID() + "-pvc",
						},
					},
				},
				{
					Name: "configmap",
					VolumeSource: corev1.VolumeSource{
						ConfigMap: &corev1.ConfigMapVolumeSource{
							LocalObjectReference: corev1.LocalObjectReference{
								Name: ip.Server.ID() + "-configmap",
							},
							DefaultMode: &[]int32{int32(0755)}[0],
						},
					},
				},
			},
			Containers: []corev1.Container{
				{
					Name:  "installer",
					Image: ip.Script.ContainerImage,
					Command: []string{
						"/mnt/install/install.sh",
					},
					Resources: corev1.ResourceRequirements{},
					VolumeMounts: []corev1.VolumeMount{
						{
							Name:      "configmap",
							MountPath: "/mnt/install",
						},
						{
							Name:      "storage",
							MountPath: "/mnt/server",
						},
					},
				},
			},
			RestartPolicy: corev1.RestartPolicy("Never"),
		},
	}

	// Env
	for _, k := range ip.Server.GetEnvironmentVariables() {
		a := strings.SplitN(k, "=", 2)

		if a[0] != "" && a[1] != "" {
			pod.Spec.Containers[0].Env = append(pod.Spec.Containers[0].Env, v1.EnvVar{Name: a[0], Value: a[1]})
		}
	}

	// cfg := config.Get()
	// if cfg.System.User.Rootless.Enabled {
	// 	conf.User = fmt.Sprintf("%d:%d", cfg.System.User.Rootless.ContainerUID, cfg.System.User.Rootless.ContainerGID)
	// }

	// Ensure the root directory for the server exists properly before attempting
	// to trigger the reinstall of the server. It is possible the directory would
	// not exist when this runs if Wings boots with a missing directory and a user
	// triggers a reinstall before trying to start the server.
	if err := ip.Server.EnsureDataDirectoryExists(); err != nil {
		return "", err
	}

	ip.Server.Log().WithField("install_script", ip.tempDir()+"/install.sh").Info("creating install container for server process")
	// Remove the temporary directory when the installation process finishes for this server container.
	defer func() {
		if err := os.RemoveAll(ip.tempDir()); err != nil {
			if !os.IsNotExist(err) {
				ip.Server.Log().WithField("error", err).Warn("failed to remove temporary data directory after install process")
			}
		}
	}()

	ip.Server.Log().WithField("container_id", ip.Server.ID()+"-installer").Info("running installation script for server in container")
	_, err = ip.clientset.CoreV1().Pods(config.Get().System.Namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return "", err
	}

	// Process the install event in the background by listening to the stream output until the
	// container has stopped, at which point we'll disconnect from it.
	//
	// If there is an error during the streaming output just report it and do nothing else, the
	// install can still run, the console just won't have any output.
	go func(id string) {
		ip.Server.Events().Publish(DaemonMessageEvent, "Starting installation process, this could take a few minutes...")

		err = ip.waitForPodRunning()
		if err != nil {
			ip.Server.Log().WithField("error", err).Warn("pod never entered running phase")
		}

		if err := ip.StreamOutput(ctx, id); err != nil {
			ip.Server.Log().WithField("error", err).Warn("error connecting to server install stream output")
		}
	}(ip.Server.ID())

	err = ip.waitForPodSucceeded()
	// Once the container has stopped running we can mark the install process as being completed.
	if err == nil {
		ip.Server.Events().Publish(DaemonMessageEvent, "Installation process completed.")
	} else {
		return "", err
	}

	return ip.Server.ID(), err
}

// StreamOutput streams the output of the installation process to a log file in
// the server configuration directory, as well as to a websocket listener so
// that the process can be viewed in the panel by administrators.
func (ip *InstallationProcess) StreamOutput(ctx context.Context, id string) error {
	req := ip.clientset.CoreV1().Pods(config.Get().System.Namespace).GetLogs(ip.Server.ID()+"-installer", &corev1.PodLogOptions{
		Follow: true,
	})
	podLogs, err := req.Stream(ctx)
	if err != nil {
		return err
	}
	defer podLogs.Close()

	err = system.ScanReader(podLogs, ip.Server.Sink(system.InstallSink).Push)
	if err != nil && !errors.Is(err, context.Canceled) {
		ip.Server.Log().WithFields(log.Fields{"container_id": id, "error": err}).Warn("error processing install output lines")
	}
	return nil
}

// resourceLimits returns the install container specific resource limits. This
// looks at the globally defined install container limits and attempts to use
// the higher of the two (defined limits & server limits). This allows for servers
// with super low limits (e.g. Discord bots with 128Mb of memory) to perform more
// intensive installation processes if needed.
//
// This also avoids a server with limits such as 4GB of memory from accidentally
// consuming 2-5x the defined limits during the install process and causing
// system instability.

// func (ip *InstallationProcess) resourceLimits() container.Resources {
// 	limits := config.Get().Docker.InstallerLimits

// 	// Create a copy of the configuration so we're not accidentally making changes
// 	// to the underlying server build data.
// 	c := *ip.Server.Config()
// 	cfg := c.Build
// 	if cfg.MemoryLimit < limits.Memory {
// 		cfg.MemoryLimit = limits.Memory
// 	}
// 	// Only apply the CPU limit if neither one is currently set to unlimited. If the
// 	// installer CPU limit is unlimited don't even waste time with the logic, just
// 	// set the config to unlimited for this.
// 	if limits.Cpu == 0 {
// 		cfg.CpuLimit = 0
// 	} else if cfg.CpuLimit != 0 && cfg.CpuLimit < limits.Cpu {
// 		cfg.CpuLimit = limits.Cpu
// 	}

// 	resources := cfg.AsContainerResources()
// 	// Explicitly remove the PID limits for the installation container. These scripts are
// 	// defined at an administrative level and users can't manually execute things like a
// 	// fork bomb during this process.
// 	resources.PidsLimit = nil

// 	return resources
// }

// SyncInstallState makes a HTTP request to the Panel instance notifying it that
// the server has completed the installation process, and what the state of the
// server is. A boolean value of "true" means everything was successful, "false"
// means something went wrong and the server must be deleted and re-created.
func (s *Server) SyncInstallState(successful bool) error {
	return s.client.SetInstallationStatus(s.Context(), s.ID(), successful)
}
