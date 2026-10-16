// Copyright IBM Corp. 2024, 2026
// SPDX-License-Identifier: BUSL-1.1

package packer

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"testing"
	"time"

	packersdk "github.com/hashicorp/packer-plugin-sdk/packer"
	pluginsdk "github.com/hashicorp/packer-plugin-sdk/plugin"
)

func helperProcess(s ...string) *exec.Cmd {
	cs := []string{"-test.run=TestHelperProcess", "--"}
	cs = append(cs, s...)
	env := []string{
		"GO_WANT_HELPER_PROCESS=1",
		"PACKER_PLUGIN_MIN_PORT=10000",
		"PACKER_PLUGIN_MAX_PORT=25000",
	}

	cmd := exec.Command(os.Args[0], cs...)
	cmd.Env = append(env, os.Environ()...)
	return cmd
}

// This is not a real test. This is just a helper process kicked off by
// tests.
func TestHelperProcess(*testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}

	defer os.Exit(0)

	args := os.Args
	for len(args) > 0 {
		if args[0] == "--" {
			args = args[1:]
			break
		}

		args = args[1:]
	}

	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "No command\n")
		os.Exit(2)
	}

	cmd, cmdArgs := args[0], args[1:]

	// useProto reports whether the emulated plugin subprocess was asked to
	// serve protobuf. It is set by the "start" form below and by a direct
	// --protobuf flag on component commands.
	useProto := false
	if cmd == "start" {
		// Emulate the SDK Set "start <kind> [--protobuf] <name>" command so
		// component factories can be tested with their full command line.
		filtered := make([]string, 0, 2)
		for _, a := range cmdArgs {
			if a == "--protobuf" {
				useProto = true
				continue
			}
			filtered = append(filtered, a)
		}
		if len(filtered) != 2 {
			fmt.Fprintf(os.Stderr, "start expects <kind> <name>, got %v\n", cmdArgs)
			os.Exit(2)
		}
		cmd, cmdArgs = filtered[0], filtered[1:]
	}
	args = cmdArgs

	switch cmd {
	case "bad-version":
		fmt.Printf("%s1|%s|tcp|:1234\n", pluginsdk.APIVersionMajor, pluginsdk.APIVersionMinor)
		<-make(chan int)
	case "builder":
		server, err := pluginsdk.Server()
		if err != nil {
			log.Printf("[ERR] %s", err)
			os.Exit(1)
		}
		// Honour --protobuf the same way the SDK Set does, so tests can
		// exercise both wire protocols against the helper plugin.
		if useProto {
			server.UseProto = true
		}
		for _, a := range args {
			if a == "--protobuf" {
				server.UseProto = true
			}
		}
		err = server.RegisterBuilder(new(packersdk.MockBuilder))
		if err != nil {
			log.Printf("[ERR] %s", err)
			os.Exit(1)
		}
		server.Serve()
	case "hook":
		server, err := pluginsdk.Server()
		if err != nil {
			log.Printf("[ERR] %s", err)
			os.Exit(1)
		}
		err = server.RegisterHook(new(packersdk.MockHook))
		if err != nil {
			log.Printf("[ERR] %s", err)
			os.Exit(1)
		}
		server.Serve()
	case "invalid-rpc-address":
		fmt.Println("lolinvalid")
	case "mock":
		fmt.Printf("%s|%s|tcp|:1234\n", pluginsdk.APIVersionMajor, pluginsdk.APIVersionMinor)
		<-make(chan int)
	case "post-processor":
		server, err := pluginsdk.Server()
		if err != nil {
			log.Printf("[ERR] %s", err)
			os.Exit(1)
		}
		err = server.RegisterPostProcessor(new(helperPostProcessor))
		if err != nil {
			log.Printf("[ERR] %s", err)
			os.Exit(1)
		}
		server.Serve()
	case "provisioner":
		server, err := pluginsdk.Server()
		if err != nil {
			log.Printf("[ERR] %s", err)
			os.Exit(1)
		}
		err = server.RegisterProvisioner(new(packersdk.MockProvisioner))
		if err != nil {
			log.Printf("[ERR] %s", err)
			os.Exit(1)
		}
		server.Serve()
	case "datasource":
		server, err := pluginsdk.Server()
		if err != nil {
			log.Printf("[ERR] %s", err)
			os.Exit(1)
		}
		err = server.RegisterDatasource(new(packersdk.MockDatasource))
		if err != nil {
			log.Printf("[ERR] %s", err)
			os.Exit(1)
		}
		server.Serve()
	case "start-timeout":
		time.Sleep(1 * time.Minute)
		os.Exit(1)
	case "stderr":
		fmt.Printf("%s|%s|tcp|:1234\n", pluginsdk.APIVersionMajor, pluginsdk.APIVersionMinor)
		log.Println("HELLO")
		log.Println("WORLD")
	case "stdin":
		fmt.Printf("%s|%s|tcp|:1234\n", pluginsdk.APIVersionMajor, pluginsdk.APIVersionMinor)
		data := make([]byte, 5)
		if _, err := os.Stdin.Read(data); err != nil {
			log.Printf("stdin read error: %s", err)
			os.Exit(100)
		}

		if string(data) == "hello" {
			os.Exit(0)
		}

		os.Exit(1)
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %q\n", cmd)
		os.Exit(2)
	}
}
