/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/cubefs/cubefs-csi/pkg/cubefs"
	"github.com/golang/glog"

	"github.com/spf13/cobra"
)

var (
	mountpoint string
	volumeName string
	version    = "1.0.0"
	conf       cubefs.Config
)

// injected while compile
var (
	CommitID  = ""
	BuildTime = ""
	Branch    = ""
)

func init() {
	_ = flag.Set("logtostderr", "true")
}

func main() {
	fmt.Printf("System build info: BuildTime: [%s], Branch [%s], CommitID [%s]\n", BuildTime, Branch, CommitID)

	cmd := &cobra.Command{
		Use:   "client-run --mountpoint=<mountpoint> --volumename=<volumename>",
		Short: "CFS Client Runner",
		Run: func(cmd *cobra.Command, args []string) {
			registerInterceptedSignal()
			handle()
		},
	}

	cmd.Flags().AddGoFlagSet(flag.CommandLine)
	cmd.PersistentFlags().StringVar(&mountpoint, "mountpoint", "", "volume's mountpoint")
	cmd.PersistentFlags().StringVar(&volumeName, "volumename", "", "volume's name")

	if err := cmd.Execute(); err != nil {
		glog.Errorf("cmd.Execute error:%v\n", err)
		os.Exit(1)
	}

	os.Exit(0)
}

func handle() {
	d, err := cubefs.NewDriver(conf)
	if err != nil {
		glog.Errorf("cubefs.NewDriver error:%v\n", err)
		os.Exit(1)
	}

	err = d.RunClient(mountpoint, volumeName)
	if err != nil {
		glog.Errorf("cfs client run error:%v\n", err)
		os.Exit(1)
	}

}

func registerInterceptedSignal() {
	sigC := make(chan os.Signal, 1)
	signal.Notify(sigC, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigC
		glog.Errorf("Killed due to a received signal (%v)\n", sig)
		os.Exit(1)
	}()
}
