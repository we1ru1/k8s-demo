package main

import (
	"flag"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"

	generatedclientset "k8s-demo/exercise4/pkg/client/clientset/versioned"
	staticsiteinformer "k8s-demo/exercise4/pkg/client/informers/externalversions/staticsite/v1alpha1"
)

func main() {
	var kubeconfig *string
	var namespace *string

	if home := homedir.HomeDir(); home != "" {
		kubeconfig = flag.String("kubeconfig", filepath.Join(home, ".kube", "config"), "absolute path to kubeconfig")
	} else {
		kubeconfig = flag.String("kubeconfig", "", "absolute path to kubeconfig")
	}
	namespace = flag.String("namespace", "default", "watch namespace")
	flag.Parse()

	cfg, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		panic(err)
	}

	kubeClient, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		panic(err)
	}

	siteClient, err := generatedclientset.NewForConfig(cfg)
	if err != nil {
		panic(err)
	}

	siteInformer := staticsiteinformer.NewStaticSiteInformer(
		siteClient,
		*namespace,
		0,
		cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
	)
	kubeInformerFactory := informers.NewSharedInformerFactoryWithOptions(kubeClient, 0, informers.WithNamespace(*namespace))
	deploymentInformer := kubeInformerFactory.Apps().V1().Deployments().Informer()

	controller := NewController(kubeClient, siteClient, siteInformer, deploymentInformer)

	stopCh := make(chan struct{})
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigCh
		close(stopCh)
	}()

	go siteInformer.Run(stopCh)
	go deploymentInformer.Run(stopCh)

	if err := controller.Run(stopCh); err != nil {
		panic(err)
	}
}
