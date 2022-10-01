package main

import (
	"context"
	"errors"
	"flag"
	"github.com/avast/retry-go"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"io"
	"log"
	"net"
	"strings"
)

func main() {
	// This is used for go servers to handle request-scoped values and handle
	// cleanup if the request is cancelled due to a timeout or the like. Neat!
	// https://go.dev/blog/context
	ctx := context.Background()

	var logDestinationAddress string
	flag.StringVar(&logDestinationAddress, "log-destination-address", "graylog.lan:5555", "Specify the address and port to send logs to, such as localhost:5555.")
	flag.Parse() // after declaring flags we need to call it

	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		panic(err)
	}

	forwarder := NewForwarder(cli, ctx, logDestinationAddress)
	forwarder.forwardRunningContainers()

	args := filters.NewArgs()
	args.Add("type", "container")
	args.Add("event", "create")
	args.Add("event", "start")
	args.Add("event", "delete")
	options := types.EventsOptions{
		Filters: args,
	}
	messages, eventErrors := cli.Events(ctx, options)

	// for in this case is an infinite loop as go doesn't have a "while"
	// statement:
	// https://gophercoding.com/while-true-loop/
	// select lets us wait until we have a message from the event listener.
	// https://go.dev/tour/concurrency/5
	for {
		select {
		case err := <-eventErrors:
			// Intentionally do not panic here, as we could still get valid
			// messages later.
			print(err)
		case msg := <-messages:
			if msg.Type == "container" {
				id := msg.ID
				if msg.Action == "create" {
					forwarder.containerStates[id] = msg.Action
				}
				if msg.Action == "delete" {
					delete(forwarder.containerStates, id)
					delete(forwarder.containerNames, id)
				}
				if msg.Action == "start" {
					forwarder.startForwarding(id)
				}
			}
		}
	}
}

// A forwarder represents a stream of log messages from Docker sent to a tcp socket.
type Forwarder struct {
	cli                   *client.Client
	ctx                   context.Context
	logDestinationAddress string
	containerNames        map[string]string
	containerStates       map[string]string
}

func NewForwarder(cli *client.Client, ctx context.Context, logDestinationAddress string) *Forwarder {
	return &Forwarder{
		cli,
		ctx,
		logDestinationAddress,
		// Store cache of container IDs to the last known state. This allows us to
		// track when a container goes from "create" to "start" versus just "start"
		// for a container that is being restarted.
		make(map[string]string),
		// Store a cache of container names, so when we emit a log message we can
		// prefix the message with the container name instead of just the ID.
		make(map[string]string),
	}
}

// Forward logs for all currently running containers. Best used when the forwarder
// is first starting and containers are already in the started state.
func (f *Forwarder) forwardRunningContainers() {
	listResponse, err := f.cli.ContainerList(f.ctx, types.ContainerListOptions{})
	// We panic here because this is called at startup, and this means we couldn't
	// connect to the docker socket at all. Once past this phase we will retry
	// connections (such as if docker itself is restarted).
	if err != nil {
		panic(err)
	}

	for _, container := range listResponse {
		f.startForwarding(container.ID)
	}
}

// Forward logs for a single container by its ID.
func (f *Forwarder) startForwarding(id string) {
	go func() {
		shouldRetry := true

		for shouldRetry {
			// Inspect the container to get a name.
			inspectResponse, err := f.cli.ContainerInspect(f.ctx, id)
			if err != nil {
				// Presumably future calls will fail, but we can still
				// reasonably use an ID if that's all we get.
				f.containerNames[id] = id
			} else {
				// Remove the leading slash from the container name.
				f.containerNames[id] = inspectResponse.Name[1:]
			}

			logsOptions := types.ContainerLogsOptions{
				ShowStdout: true,
				ShowStderr: true,
				Follow:     true,
			}

			// The previous state was not "create a new container", so
			// only log new messages. Presumably it's possible to lose
			// messages, but personally I prefer that to large numbers
			// of duplicate messages.
			if f.containerStates[id] != "create" {
				logsOptions.Tail = "0"
			}

			// Since we've figured out our tail option we can update the action.
			f.containerStates[id] = "start"

			logStream, err := f.cli.ContainerLogs(f.ctx, id, logsOptions)
			if err != nil {
				if client.IsErrNotFound(err) {
					// The container has been removed, so we can't do anything to get logs.
					return
				}
				panic(err)
			}

			// Good reference at https://www.linode.com/docs/guides/developing-udp-and-tcp-clients-and-servers-in-go/
			var c net.Conn

			// The retry library handles backoff and retry logic for us. However, only the untagged "4.x"
			// branch contains an infinite retry feature, so we wrap this in a loop as well.
			for c == nil {
				_ = retry.Do(
					func() error {
						c, err = net.Dial("tcp", f.logDestinationAddress)
						if err != nil {
							log.Println(err.Error())
							return err
						}
						return nil
					},
				)
			}
			defer func(c net.Conn) {
				log.Println("Log connection closed for container " + f.containerNames[id])
				_ = c.Close()
			}(c)

			// For every message logged, prepend 'container_name ' to
			// the message. We have to use StdCopy from the Docker SDK to
			// demultiplex the messages, as they are both stdout and stderr
			// on the same channel.
			log.Println("Log forwarding started for container " + f.containerNames[id])
			_, err = stdcopy.StdCopy(NewPrependWriter(c, f.containerNames[id]), NewPrependWriter(c, f.containerNames[id]), logStream)
			if err != nil {
				opErr := new(net.OpError)
				if errors.Is(err, opErr) {
					log.Println("Forwarding from container " + f.containerNames[id] + " was interrupted. Will retry...")
				}
			}

			defer func(out io.ReadCloser) {
				// The container has exited, so do not retry.
				log.Println("Container " + f.containerNames[id] + " has exited.")
				shouldRetry = false
				err := out.Close()
				if err != nil {
					panic(err)
				}
			}(logStream)
		}
	}()
}

// Good example of "decorating a writer" at https://forum.golangbridge.org/t/decorating-a-method/6475/9
type PrependWriter struct {
	w    io.Writer
	name string
}

func NewPrependWriter(w io.Writer, name string) *PrependWriter {
	return &PrependWriter{w, name}
}

// Add the container name or ID to the beginning of a string.
func (pw *PrependWriter) Write(p []byte) (int, error) {
	var s strings.Builder
	s.WriteString(pw.name + " ")
	s.Write(p)
	n, err := pw.w.Write([]byte(s.String()))

	// We first check that the entire string we wrote was written to the
	// underlying writer.
	if n != s.Len() {
		return 0, err
	}

	// However, callers like the Docker SDK's stdcopy.StdCopy() may check that
	// their value was also written correctly, checking its length. If we return
	// the length of the total string they may error out themselves, because we
	// have written more than they expected. Instead, only return the length of
	// the passed in byte slice.
	return len(p), err
}
