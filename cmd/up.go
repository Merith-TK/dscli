package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"github.com/bwmarrin/discordgo"
	"github.com/darenliang/dscli/common"
	"github.com/schollz/progressbar/v3"
	"github.com/spf13/cobra"
)

var (
	upDebug  bool
	upResume bool
)

// upCmd represents the up command
var upCmd = &cobra.Command{
	Use:        "up <local file> <remote file>",
	Example:    "up test.txt test.txt",
	SuggestFor: []string{"upload"},
	Short:      "Upload file",
	Args: func(cmd *cobra.Command, args []string) error {
		if len(args) < 1 {
			return errors.New("requires at least one argument")
		}
		return nil
	},
	RunE: up,
}

func init() {
	upCmd.Flags().BoolVarP(&upDebug, "debug", "d", false, "debug mode: <total bytes> <bytes uploaded>")
	upCmd.Flags().BoolVarP(&upResume, "resume", "r", false, "resume upload")

	rootCmd.AddCommand(upCmd)
}

// up command handler
func up(cmd *cobra.Command, args []string) error {
	session, guild, channels, err := common.GetDiscordSession()
	if err != nil {
		return err
	}
	defer session.Close()

	fileMap, err := common.ParseFileMap(channels)
	if err != nil {
		return err
	}

	// check if max Discord channel limit is reached
	if !upResume && len(fileMap) >= common.MaxDiscordChannels {
		return fmt.Errorf(
			"max Discord channel limit of %d is reached",
			common.MaxDiscordChannels,
		)
	}

	local := args[0] // local filename

	// open local file to upload
	localFile, err := os.Open(local)
	if err != nil {
		return err
	}
	defer localFile.Close()

	_, localBase := filepath.Split(local)

	var remote string // remote filename
	if len(args) == 1 {
		remote = localBase
	} else {
		remote = args[1]
	}

	// remote filename already exists
	if _, ok := fileMap[remote]; ok && !upResume {
		return fmt.Errorf("%s already exists on Discord", remote)
	} else if !ok && upResume {
		return fmt.Errorf("%s does not exist on Discord", remote)
	}

	// get size of local file
	stat, err := localFile.Stat()
	if err != nil {
		return err
	}
	size := stat.Size()
	sizeStr := strconv.FormatInt(size, 10)

	// get max Discord file size
	maxDiscordFileSize, err := common.GetMaxFileSizeUpload(session, guild)
	if err != nil {
		return err
	}

	var channel *discordgo.Channel
	blockNumber := 0

	if upResume {
		channel = fileMap[remote]

		if channel.Topic != sizeStr {
			return errors.New("remote file size does not match local file size")
		}

		msgs, err := session.ChannelMessages(channel.ID, 1, "", "0", "")
		if err != nil {
			return err
		}

		if len(msgs) == 0 || len(msgs[0].Attachments) == 0 {
			return errors.New("cannot infer block size")
		}

		if msgs[0].Attachments[0].Size > int(maxDiscordFileSize) {
			return fmt.Errorf(
				"inferred block size %d is larger than the largest permitted block size %d",
				msgs[0].Attachments[0].Size,
				maxDiscordFileSize,
			)
		}

		maxDiscordFileSize = msgs[0].Attachments[0].Size

		msgs, err = session.ChannelMessages(channel.ID, 2, "", "", "")
		if err != nil {
			return err
		}

		for _, msg := range msgs {
			if len(msg.Attachments) == 0 {
				continue
			}
			if msg.Attachments[0].Size != int(maxDiscordFileSize) {
				return errors.New("complete upload inferred from incomplete last block")
			}
			blockNumber, err = strconv.Atoi(msg.Attachments[0].Filename)
			if err != nil {
				return err
			}
			break
		}

		if int64(blockNumber*maxDiscordFileSize) == size {
			return errors.New("upload is already complete")
		}

	} else {
		// encode remote filename
		encodedRemote, err := common.EncodeFilename(remote)
		if err != nil {
			return err
		}

		// create channel for file
		channel, err = session.GuildChannelCreate(guild.ID, encodedRemote, discordgo.ChannelTypeGuildText)
		if err != nil {
			return fmt.Errorf("cannot create remote file: %v", err)
		}

		// set channel topic to filesize
		channelSettings := &discordgo.ChannelEdit{
			Topic: sizeStr,
		}
		// ignore if errored since it is not critical
		_, _ = session.ChannelEdit(channel.ID, channelSettings)
	}

	// Check if file needs chunking
	if size <= int64(maxDiscordFileSize) {
		return uploadSingleFile(session, channel, localFile, localBase, size, upDebug, upResume)
	}
	return uploadChunkedFile(session, channel, localFile, localBase, size, int64(maxDiscordFileSize), upDebug, upResume, blockNumber)
}

// uploadSingleFile handles files that fit within Discord's size limit
func uploadSingleFile(session *discordgo.Session, channel *discordgo.Channel,
	file *os.File, filename string, size int64, debug bool, resume bool) error {

	// Read entire file
	data, err := io.ReadAll(file)
	if err != nil {
		return fmt.Errorf("failed to read file: %v", err)
	}

	var bar *progressbar.ProgressBar
	if !debug {
		bar = progressbar.DefaultBytes(size, "Uploading "+filename)
	}

	msg := &discordgo.MessageSend{
		Files: []*discordgo.File{
			{
				Name:   filename,
				Reader: bytes.NewReader(data),
			},
		},
	}

	// Send file with retries
	var message *discordgo.Message
	maxUploadTries := 10
	for i := 0; i < maxUploadTries; i++ {
		message, err = session.ChannelMessageSendComplex(channel.ID, msg)
		if err != nil {
			if i == maxUploadTries-1 {
				return fmt.Errorf("failed to upload file after %d attempts: %v", maxUploadTries, err)
			}
			continue
		}
		break
	}

	if !resume {
		_ = session.ChannelMessagePin(channel.ID, message.ID)
	}

	if !debug && bar != nil {
		bar.Add64(size)
	}

	return nil
}

// uploadChunkedFile handles files that exceed Discord's size limit
func uploadChunkedFile(session *discordgo.Session, channel *discordgo.Channel,
	file *os.File, filename string, size, maxChunkSize int64, debug bool, resume bool, startBlock int) error {

	// Calculate safe chunk size (50 bytes less than max)
	chunkSize := maxChunkSize - 50
	if chunkSize <= 0 {
		return errors.New("calculated chunk size is too small")
	}

	buf := make([]byte, chunkSize)
	blockNumber := startBlock

	var bar *progressbar.ProgressBar
	if !debug {
		bar = progressbar.DefaultBytes(size, "Uploading "+filename)
		_ = bar.Add(blockNumber * int(chunkSize))
	}

	first := !resume

	for {
		blockNumber++

		// Read chunk
		n, err := file.Read(buf)
		if err != nil && err != io.EOF {
			return fmt.Errorf("failed to read chunk %d: %v", blockNumber, err)
		}

		// Skip empty chunks
		if n == 0 {
			break
		}

		// Create message with chunk
		msg := &discordgo.MessageSend{
			Files: []*discordgo.File{
				{
					Name:   strconv.Itoa(blockNumber),
					Reader: bytes.NewReader(buf[:n]),
				},
			},
		}

		// Send chunk with retries
		var message *discordgo.Message
		maxUploadTries := 10
		for i := 0; i < maxUploadTries; i++ {
			message, err = session.ChannelMessageSendComplex(channel.ID, msg)
			if err != nil {
				if i == maxUploadTries-1 {
					return fmt.Errorf("failed to upload chunk %d: %v", blockNumber, err)
				}
				continue
			}
			break
		}

		// Update progress
		if !debug && bar != nil {
			bar.Add(n)
		} else if debug {
			offset, _ := file.Seek(0, io.SeekCurrent)
			fmt.Printf("%d %d\n", size, offset)
		}

		// Pin first message if new upload
		if first {
			_ = session.ChannelMessagePin(channel.ID, message.ID)
			first = false
		}

		// Check if we've reached the end
		if n < len(buf) || err == io.EOF {
			break
		}
	}

	return nil
}
