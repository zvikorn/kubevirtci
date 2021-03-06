package docker

import (
	"bufio"
	"context"
	"fmt"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	"golang.org/x/crypto/ssh/terminal"
	"io"
	"os"
	"os/signal"
	"strings"
)

func GetPrefixedContainers(cli *client.Client, prefix string) ([]types.Container, error) {
	args, err := filters.ParseFlag("name="+prefix, filters.NewArgs())
	if err != nil {
		return nil, err
	}
	containers, err := cli.ContainerList(context.Background(), types.ContainerListOptions{
		Filters: args,
		All:     true,
	})
	return containers, err
}

func GetPrefixedVolumes(cli *client.Client, prefix string) ([]*types.Volume, error) {
	args, err := filters.ParseFlag("name="+prefix, filters.NewArgs())
	if err != nil {
		return nil, err
	}
	volumes, err := cli.VolumeList(context.Background(), args)
	if err != nil {
		return nil, err
	}
	return volumes.Volumes, nil
}

func GetDDNSMasqContainer(cli *client.Client, prefix string) (*types.Container, error) {
	containers, err := GetPrefixedContainers(cli, prefix+"-"+"dnsmasq")
	if err != nil {
		return nil, err
	}

	if len(containers) == 1 {
		return &containers[0], nil
	}

	return nil, fmt.Errorf("Could not identify dnsmasq container %s", prefix+"-dnsmasq")
}

func Exec(cli *client.Client, container string, args []string, out io.Writer) (bool, error) {
	ctx := context.Background()
	id, err := cli.ContainerExecCreate(ctx, container, types.ExecConfig{
		Privileged:   true,
		Tty:          true,
		Detach:       false,
		Cmd:          args,
		AttachStdout: true,
		AttachStderr: true,
	})

	if err != nil {
		return false, err
	}

	attached, err := cli.ContainerExecAttach(ctx, id.ID, types.ExecConfig{
		AttachStderr: true,
		AttachStdout: true,
		Tty:          true,
	})
	if err != nil {
		return false, err
	}
	defer attached.Close()

	io.Copy(out, attached.Reader)

	resp, err := cli.ContainerExecInspect(ctx, id.ID)
	if err != nil {
		return false, err
	}
	return resp.ExitCode == 0, nil
}

func Terminal(cli *client.Client, container string, args []string, file *os.File) (int, error) {

	ctx := context.Background()
	id, err := cli.ContainerExecCreate(ctx, container, types.ExecConfig{
		Privileged:   true,
		Tty:          terminal.IsTerminal(int(file.Fd())),
		Detach:       false,
		Cmd:          args,
		AttachStdout: true,
		AttachStderr: true,
		AttachStdin:  true,
	})

	if err != nil {
		return -1, err
	}

	attached, err := cli.ContainerExecAttach(ctx, id.ID, types.ExecConfig{
		AttachStderr: true,
		AttachStdout: true,
		AttachStdin:  true,
		Tty:          terminal.IsTerminal(int(file.Fd())),
	})
	if err != nil {
		return -1, err
	}
	defer attached.Close()

	if terminal.IsTerminal(int(file.Fd())) {
		state, err := terminal.MakeRaw(int(file.Fd()))
		if err != nil {
			return -1, err
		}

		errChan := make(chan error)

		go func() {
			interrupt := make(chan os.Signal, 1)
			signal.Notify(interrupt, os.Interrupt)
			<-interrupt
			close(errChan)
		}()

		go func() {
			_, err := io.Copy(file, attached.Conn)
			errChan <- err
		}()

		go func() {
			_, err := io.Copy(attached.Conn, file)
			errChan <- err
		}()

		defer func() {
			terminal.Restore(int(file.Fd()), state)
		}()

		err = <-errChan

		if err != nil {
			return -1, err
		}
	}

	resp, err := cli.ContainerExecInspect(ctx, id.ID)
	if err != nil {
		return -1, err
	}
	return resp.ExitCode, nil
}

func NewCleanupHandler(cli *client.Client, errWriter io.Writer) (containers chan string, volumes chan string, done chan error) {

	ctx := context.Background()

	containers = make(chan string)
	volumes = make(chan string)
	done = make(chan error)

	go func() {
		createdContainers := []string{}
		createdVolumes := []string{}

		for {
			select {
			case container := <-containers:
				createdContainers = append(createdContainers, container)
			case volume := <-volumes:
				createdVolumes = append(createdVolumes, volume)
			case err := <-done:
				if err != nil {
					for _, c := range createdContainers {
						err := cli.ContainerRemove(ctx, c, types.ContainerRemoveOptions{Force: true})
						fmt.Printf("container: %v\n", c)
						if err != nil {
							fmt.Fprintf(errWriter, "%v\n", err)
						}
					}

					for _, v := range createdVolumes {
						err := cli.VolumeRemove(ctx, v, true)
						fmt.Printf("volume: %v\n", v)
						if err != nil {
							fmt.Fprintf(errWriter, "%v\n", err)
						}
					}
				}
			}
		}
	}()

	return
}

func PrintProgress(progressReader io.ReadCloser, writer *os.File) {
	isTerminal := terminal.IsTerminal(int(writer.Fd()))
	w, _, err := terminal.GetSize(int(writer.Fd()))

	if isTerminal && err == nil {
		scanner := bufio.NewScanner(progressReader)
		for scanner.Scan() {
			line := scanner.Text()
			clearLength := w - len(line)
			if clearLength < 0 {
				clearLength = 0
			}
			fmt.Print("\r" + line + strings.Repeat(" ", clearLength))
		}
	} else {
		fmt.Fprint(writer, "Downloading ...")
		scanner := bufio.NewScanner(progressReader)
		for scanner.Scan() {
			fmt.Print(".")
		}
		fmt.Print("\n")
	}
}
