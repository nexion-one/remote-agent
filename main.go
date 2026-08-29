package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage: nexion-remote-agent <version|proxy|daemon> [flags]\n")
		os.Exit(1)
	}

	switch os.Args[1] {
	case "version":
		fmt.Printf("nexion-remote-agent %s (protocol %d)\n", AgentVersion, ProtocolVersion)

	case "daemon":
		project := flagValue(os.Args[2:], "--project")
		if project == "" {
			fmt.Fprintf(os.Stderr, "error: --project is required\n")
			os.Exit(1)
		}
		runDaemon(project)

	case "proxy":
		project := flagValue(os.Args[2:], "--project")
		if project == "" {
			fmt.Fprintf(os.Stderr, "error: --project is required\n")
			os.Exit(1)
		}
		runProxy(project)

	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", os.Args[1])
		os.Exit(1)
	}
}

func flagValue(args []string, name string) string {
	for i, arg := range args {
		if arg == name && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}
