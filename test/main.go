package main

import "fmt"

func main() {
	stopCh := make(chan struct{})

	go func() {
		for i := 0; i < 3; i++ {
			stopCh <- struct{}{}
		}

	}()

	for i := 0; i < 3; i++ {
		elem := stopCh
		fmt.Println("elem: ", elem)
	}
}
