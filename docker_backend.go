package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"strconv"
	"strings"

	"github.com/flynn/flynn-host/ports"
	"github.com/flynn/flynn-host/types"
	"github.com/flynn/go-dockerclient"
	"github.com/technoweenie/grohl"
)

/*
func NewDockerBackend(state *State, bindAddr string) (Backend, error) {
	dockerc, err := docker.NewClient("unix:///var/run/docker.sock")
	if err != nil {
		return nil, err
	}
	d := &DockerBackend{
		state:    state,
		ports:    ports.NewAllocator(55000, 65535),
		docker:   dockerc,
		bindAddr: bindAddr,
	}
	go d.handleEvents()
	return d, nil
}
*/

type DockerBackend struct {
	docker dockerClient
	state  *State
	ports  *ports.Allocator

	bindAddr string
}

type dockerClient interface {
	PullImage(docker.PullImageOptions, io.Writer) error
	CreateContainer(*docker.Config) (*docker.Container, error)
	StartContainer(string, *docker.HostConfig) error
	InspectContainer(string) (*docker.Container, error)
	Events() (*docker.EventStream, error)
	StopContainer(string, uint) error
	ResizeContainerTTY(string, int, int) error
	AttachToContainer(docker.AttachToContainerOptions) error
	KillContainer(string) error
	ListContainers(docker.ListContainersOptions) ([]docker.APIContainers, error)
}

func (d *DockerBackend) Run(job *host.Job) error {
	g := grohl.NewContext(grohl.Data{"backend": "docker", "fn": "run", "job.id": job.ID})
	g.Log(grohl.Data{"at": "start", "job.artifact.url": job.Artifact.URI, "job.cmd": job.Config.Cmd})

	config := &docker.Config{
		// Image:
		Entrypoint:   job.Config.Entrypoint,
		Cmd:          job.Config.Cmd,
		Tty:          job.Config.TTY,
		AttachStdout: true,
		AttachStderr: true,
		AttachStdin:  job.Config.Stdin,
		StdinOnce:    job.Config.Stdin,
		ExposedPorts: make(map[string]struct{}, len(job.Config.Ports)),
	}
	hostConfig := &docker.HostConfig{
		PortBindings: make(map[string][]docker.PortBinding, len(job.Config.Ports)),
	}
	config.Env = make([]string, 0, len(job.Config.Env)+len(job.Config.Ports)+1)
	for k, v := range job.Config.Env {
		config.Env = append(config.Env, k+"="+v)
	}

	for i, p := range job.Config.Ports {
		if p.Port == 0 {
			port, err := d.ports.Get()
			if err != nil {
				return err
			}
			p.Port = int(port)
		}
		port := strconv.Itoa(p.Port)

		if i == 0 {
			config.Env = append(config.Env, "PORT="+port)
		}
		config.Env = append(config.Env, fmt.Sprintf("PORT_%d=%s", i, port))
		config.ExposedPorts[port+"/"+p.Proto] = struct{}{}
		hostConfig.PortBindings[port+"/"+p.Proto] = []docker.PortBinding{{HostPort: port, HostIp: d.bindAddr}}
	}

	if strings.HasPrefix(job.ID, "flynn-") {
		config.Name = job.ID
	} else {
		config.Name = "flynn-" + job.ID
	}

	d.state.AddJob(job)
	g.Log(grohl.Data{"at": "create_container"})
	container, err := d.docker.CreateContainer(config)
	if err == docker.ErrNoSuchImage {
		g.Log(grohl.Data{"at": "pull_image"})
		err = d.docker.PullImage(docker.PullImageOptions{Repository: config.Image}, os.Stdout)
		if err != nil {
			g.Log(grohl.Data{"at": "pull_image", "status": "error", "err": err})
			return err
		}
		container, err = d.docker.CreateContainer(config)
	}
	if err != nil {
		g.Log(grohl.Data{"at": "create_container", "status": "error", "err": err})
		return err
	}
	d.state.SetContainerID(job.ID, container.ID)
	d.state.WaitAttach(job.ID)
	g.Log(grohl.Data{"at": "start_container"})
	if err := d.docker.StartContainer(container.ID, hostConfig); err != nil {
		g.Log(grohl.Data{"at": "start_container", "status": "error", "err": err})
		return err
	}
	container, err = d.docker.InspectContainer(container.ID)
	if err != nil {
		g.Log(grohl.Data{"at": "inspect_container", "status": "error", "err": err})
		return err
	}
	d.state.SetInternalIP(job.ID, container.NetworkSettings.IPAddress)
	g.Log(grohl.Data{"at": "finish"})
	return nil
}

func (d *DockerBackend) Stop(id string) error {
	const stopTimeout = 1
	return d.docker.StopContainer(id, stopTimeout)
}

func (d *DockerBackend) RestoreState(jobs map[string]*host.ActiveJob, dec *json.Decoder) error {
	return nil
}

/*
func (d *DockerBackend) Attach(req *AttachRequest) error {
	opts := docker.AttachToContainerOptions{
		Container:    req.Job.ContainerID,
		OutputStream: req.ReadWriter,
		Logs:         req.Logs,
		Stream:       req.Stream,
		Success:      req.Attached,
	}
	for _, s := range req.Streams {
		switch s {
		case "stdout":
			opts.Stdout = true
		case "stderr":
			opts.Stderr = true
		case "stdin":
			opts.Stdin = true
			opts.InputStream = req.ReadWriter
		}
	}

	if req.Job.Job.Config.TTY && opts.Stdin {
		resize := func() { d.docker.ResizeContainerTTY(req.Job.ContainerID, req.Height, req.Width) }
		if req.Job.Status == host.StatusRunning {
			resize()
		} else {
			var once sync.Once
			go func() {
				ch := make(chan host.Event)
				d.state.AddListener(req.Job.Job.ID, ch)
				go func() {
					// There is a race that can result in the listener being
					// added after the container has started, so check the
					// status *after* subscribing.
					// This can deadlock if we try to get a state lock while an
					// event is being sent on the listen channel, so we do it
					// in the goroutine and wrap in a sync.Once.
					j := d.state.GetJob(req.Job.Job.ID)
					if j.Status == host.StatusRunning {
						once.Do(resize)
					}
				}()
				defer d.state.RemoveListener(req.Job.Job.ID, ch)
				for event := range ch {
					if event.Event == "start" {
						once.Do(resize)
						return
					}
					if event.Event == "stop" {
						return
					}
				}
			}()
		}
	}

	return d.docker.AttachToContainer(opts)
}
*/

func (d *DockerBackend) handleEvents() {
	stream, err := d.docker.Events()
	if err != nil {
		log.Fatal(err)
	}
	for event := range stream.Events {
		if event.Status != "die" {
			continue
		}
		container, err := d.docker.InspectContainer(event.ID)
		if err != nil {
			log.Println("inspect container", event.ID, "error:", err)
			// TODO: set job status anyway?
			continue
		}
		// TODO: return ports to pool
		d.state.SetContainerStatusDone(event.ID, container.State.ExitCode)
	}
}

func (d *DockerBackend) Cleanup() error {
	g := grohl.NewContext(grohl.Data{"backend": "docker", "fn": "cleanup"})
	g.Log(grohl.Data{"at": "start"})
	containers, err := d.docker.ListContainers(docker.ListContainersOptions{})
	if err != nil {
		g.Log(grohl.Data{"at": "list", "status": "error", "err": err})
		return err
	}
outer:
	for _, c := range containers {
		for _, name := range c.Names {
			if strings.HasPrefix(name, "/flynn-") {
				g.Log(grohl.Data{"at": "kill", "container.id": c.ID, "container.name": name})
				if err := d.docker.KillContainer(c.ID); err != nil {
					g.Log(grohl.Data{"at": "kill", "container.id": c.ID, "container.name": name, "status": "error", "err": err})
				}
				continue outer
			}
		}
	}
	g.Log(grohl.Data{"at": "finish"})
	return nil
}

func appendUnique(s []string, vars ...string) []string {
outer:
	for _, v := range vars {
		for _, existing := range s {
			if strings.HasPrefix(existing, strings.SplitN(v, "=", 2)[0]+"=") {
				continue outer
			}
		}
		s = append(s, v)
	}
	return s
}
