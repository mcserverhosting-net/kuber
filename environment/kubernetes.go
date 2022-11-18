package environment

import (
	"github.com/kubectyl/kuber/config"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func Kubernetes() (c *rest.Config, clientset *kubernetes.Clientset, err error) {
	c = &rest.Config{
		Host:            config.Get().System.Host,
		BearerToken:     config.Get().System.BearerToken,
		TLSClientConfig: rest.TLSClientConfig{Insecure: true},
	}

	client, err := kubernetes.NewForConfig(c)
	if err != nil {
		panic(err)
	}
	return c, client, err
}
