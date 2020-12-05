package cmd

import (
	"errors"
	"github.com/darenliang/dscli/common"
	"github.com/spf13/cobra"
)

// rmCmd represents the rm command
var rmCmd = &cobra.Command{
	Use:        "rm file",
	Example:    "rm example.txt",
	SuggestFor: []string{"remove", "delete"},
	Short:      "Remove file",
	Args: func(cmd *cobra.Command, args []string) error {
		if len(args) < 1 {
			return errors.New("requires one argument")
		}
		return nil
	},
	RunE: rm,
}

func init() {
	rootCmd.AddCommand(rmCmd)
}

// rm command handler
func rm(cmd *cobra.Command, args []string) error {
	filename := args[0]

	session, _, channels, err := common.GetDiscordSession()
	if err != nil {
		return err
	}
	defer session.Close()

	fileMap, err := common.ParseFileMap(channels)
	if err != nil {
		return err
	}

	// old file exists
	if channel, ok := fileMap[filename]; ok {
		// remove file (via channel delete)
		_, err = session.ChannelDelete(channel.ID)
		if err != nil {
			return errors.New("cannot delete file: " + err.Error())
		}
		return nil
	} else {
		return errors.New(filename + " not found")
	}
}
