package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

func main() {
	var kubeconfig *string
	if home := homedir.HomeDir(); home != "" {
		kubeconfig = flag.String("kubeconfig", filepath.Join(home, ".kube", "config"), "(optional) absolute path to the kubeconfig file")
	} else {
		kubeconfig = flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
	}

	flag.Parse()

	config, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		panic(err)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err)
	}

	// 列出default ns下的Pod名称和状态
	fmt.Printf("Listing pods in namespace %q:\n", apiv1.NamespaceDefault)

	podsClient := clientset.CoreV1().Pods(apiv1.NamespaceDefault)      // 创建podsClient
	list, err := podsClient.List(context.TODO(), metav1.ListOptions{}) // List Pod
	if err != nil {
		panic(err)
	}

	for _, d := range list.Items {
		fmt.Printf("* Pod name: %s, status: %s\n", d.Name, d.Status.Phase)
	}

	// prompt()

	// 删除指定名称的Pod.

}

func prompt() {
	fmt.Printf("-> Press Return key to continue.")
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		break
	}
	if err := scanner.Err(); err != nil {
		panic(err)
	}
	fmt.Println()
}
