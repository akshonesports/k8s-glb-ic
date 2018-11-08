package main

import (
	"context"
	"flag"
	"io/ioutil"
	"log"
	"os"
	"os/user"
	"path/filepath"

	"cloud.google.com/go/compute/metadata"
	"golang.org/x/oauth2/google"
	"k8s.io/client-go/kubernetes"
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

var (
	namespace = flag.String("namespace", "", "namespace")
	project   = flag.String("project", "", "gcp project")
)

func getconfig() (*rest.Config, error) {
	config, err := rest.InClusterConfig()
	if err != rest.ErrNotInCluster {
		return config, err
	}

	val, desc := "", "(optional) absolute path to the kubeconfig file"
	if path := os.Getenv("KUBECONFIG"); path != "" {
		val = path
	} else if u, err := user.Current(); err == nil && u.HomeDir != "" {
		val = filepath.Join(u.HomeDir, ".kube", "config")
	} else {
		desc = "absolute path to the kubeconfig file"
	}

	kubeconfig := flag.String("kubeconfig", val, desc)

	flag.Parse()

	return clientcmd.BuildConfigFromFlags("", *kubeconfig)
}

func getnamespace(def string) string {
	if *namespace != "" {
		return *namespace
	}

	f, err := os.Open("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	if err != nil {
		return def
	}

	b, err := ioutil.ReadAll(f)
	if err != nil {
		return def
	}

	return string(b)
}

func Start(ctx context.Context) error {
	config, err := getconfig()
	if err != nil {
		return err
	}

	hc, err := google.DefaultClient(ctx, "https://www.googleapis.com/auth/compute")
	if err != nil {
		return err
	}

	project := *project
	if project == "" && metadata.OnGCE() {
		project, _ = metadata.ProjectID()
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return err
	}

	ctrl, err := NewController(clientset, project, hc)
	if err != nil {
		return err
	}

	return ctrl.Run(ctx)
}

func main() {
	ctx := context.Background()
	if err := Start(ctx); err != nil {
		log.Printf("%+v", err)
	}
}
