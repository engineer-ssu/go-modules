package main

import (
	"fmt"

	"github.com/engineer-ssu/go-modules/config"
)

var cfg *config.Config

func init() {
	cfg = config.GetConfig()
}

func main() {
	fmt.Println("main")
	fmt.Println("v0.0.3")
}
