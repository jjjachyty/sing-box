package main

import (
	"github.com/sagernet/sing-box/agent"
	"github.com/sagernet/sing-box/log"

	"github.com/spf13/cobra"
)

var agentConfigPath string

var commandAgent = &cobra.Command{
	Use:   "agent",
	Short: "Run node agent with sing-box embedded",
	Run: func(cmd *cobra.Command, args []string) {
		err := agent.Run(agentConfigPath)
		if err != nil {
			log.Fatal(err)
		}
	},
}

func init() {
	commandAgent.Flags().StringVarP(&agentConfigPath, "config", "c", "node.yaml", "set node configuration file path")
	mainCommand.AddCommand(commandAgent)
}
