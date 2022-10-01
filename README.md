# Docker Log Forwarder

This tool uses the Docker API to capture logs and forward them to a remote system.

While normally it's best to configure [syslog forwarding](https://docs.docker.com/config/containers/logging/syslog/), some systems (such as the [Home Assistant Operating System](https://github.com/home-assistant/operating-system) don't allow
Docker to be configured that way. This tool allows for logs to be forwarded from within a container, as long as that container has access to the Docker socket.

```console
go run forwarder.go --help
Usage of /var/folders/kh/l4v73x014452yvc02s48c3y40000gn/T/go-build1527857121/b001/exe/forwarder:
  -log-destination-address string
    	Specify the address and port to send logs to, such as localhost:5555. (default "graylog.lan:5555")
```
