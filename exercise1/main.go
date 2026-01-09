package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

func main() {
	var kubeconfig *string
	var podName *string
	// flag.String(arg1, arg2, arg3)的三个参数意思分别是：标志名，默认值，说明。
	// 其返回的不是一个 string，而是一个 *string (字符串指针)

	if home := homedir.HomeDir(); home != "" {
		kubeconfig = flag.String("kubeconfig", filepath.Join(home, ".kube", "config"), "(optional) absolute path to the kubeconfig file")
	} else {
		kubeconfig = flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
	}

	podName = flag.String("pod", "", "pod name to be deleted")

	flag.Parse()

	config, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		panic(err)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err)
	}

	// List出default ns下的Pod名称和状态
	podsClient := clientset.CoreV1().Pods("default")                   // 创建podsClient
	list, err := podsClient.List(context.TODO(), metav1.ListOptions{}) // List Pod
	if err != nil {
		panic(err)
	}

	// 如果podName不为空，并且namespace下确实存在，如果存在则删除该pod。
	// 否则直接输出default namespace下的所有pod名称和状态
	if *podName != "" {
		for _, d := range list.Items {
			// pod存在，删除
			if d.Name == *podName {
				fmt.Printf("Pod: %s(Phase = %s)  exists.\nNow deleting it...\n", *podName, d.Status.Phase)

				if err := podsClient.Delete(context.TODO(), *podName, metav1.DeleteOptions{}); err != nil {
					panic(err)
				}
				fmt.Println("Delete success!")

				return
			}

		}
		// pod不存在
		fmt.Printf("Pod: %s doesn't exists.\n", *podName)
	}

	// 输出default namespace下的所有Pod名称和状态
	fmt.Printf("Now list pods in namespace %q:\n", "default")
	for _, d := range list.Items {
		fmt.Printf("* Pod name: %s, status: %s\n", d.Name, d.Status.Phase)
	}

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
