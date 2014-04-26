package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/flynn/flynn-host/sampi"
	"github.com/flynn/flynn-host/types"
	"github.com/flynn/go-discoverd"
	"github.com/flynn/go-dockerclient"
	"github.com/flynn/go-flynn/attempt"
	"github.com/flynn/go-flynn/cluster"
	rpc "github.com/flynn/rpcplus/comborpc"
	"github.com/technoweenie/grohl"
)

// Attempts is the attempt strategy that is used to connect to discoverd.
var Attempts = attempt.Strategy{
	Min:   5,
	Total: 5 * time.Second,
	Delay: 200 * time.Millisecond,
}

// A command line flag to accumulate multiple key-value pairs into Attributes,
// e.g. flynn-host -attribute foo=bar -attribute bar=foo
type AttributeFlag map[string]string

func (a AttributeFlag) Set(val string) error {
	kv := strings.SplitN(val, "=", 2)
	a[kv[0]] = kv[1]
	return nil
}

func (a AttributeFlag) String() string {
	res := make([]string, 0, len(a))
	for k, v := range a {
		res = append(res, k+"="+v)
	}
	return strings.Join(res, ", ")
}

func main() {
	hostname, _ := os.Hostname()
	externalAddr := flag.String("external", "", "external IP of host")
	bindAddr := flag.String("bind", "", "bind containers to this IP")
	configFile := flag.String("config", "", "configuration file")
	manifestFile := flag.String("manifest", "/etc/flynn-host.json", "manifest file")
	hostID := flag.String("id", hostname, "host id")
	force := flag.Bool("force", false, "kill all containers booted by flynn-host before starting")
	attributes := make(AttributeFlag)
	flag.Var(&attributes, "attribute", "key=value pair to add as an attribute")
	flag.Parse()
	grohl.AddContext("app", "lorne")
	grohl.Log(grohl.Data{"at": "start"})
	g := grohl.NewContext(grohl.Data{"fn": "main"})

	dockerc, err := docker.NewClient("unix:///var/run/docker.sock")
	if err != nil {
		log.Fatal(err)
	}

	if *force {
		if err := killExistingContainers(dockerc); err != nil {
			os.Exit(1)
		}
	}

	state := NewState()
	ports := make(chan int)

	go allocatePorts(ports, 55000, 65535)
	go serveHTTP(&Host{state: state, docker: dockerc}, &attachHandler{state: state, docker: dockerc})
	go streamEvents(dockerc, state)

	processor := &jobProcessor{
		externalAddr: *externalAddr,
		bindAddr:     *bindAddr,
		docker:       dockerc,
		state:        state,
		discoverd:    os.Getenv("DISCOVERD"),
	}

	runner := &manifestRunner{
		env:        parseEnviron(),
		externalIP: *externalAddr,
		ports:      ports,
		processor:  processor,
		docker:     dockerc,
	}

	var disc *discoverd.Client
	if *manifestFile != "" {
		var r io.Reader
		var f *os.File
		if *manifestFile == "-" {
			r = os.Stdin
		} else {
			f, err = os.Open(*manifestFile)
			if err != nil {
				log.Fatal(err)
			}
			r = f
		}
		services, err := runner.runManifest(r)
		if err != nil {
			log.Fatal(err)
		}
		if f != nil {
			f.Close()
		}

		if d, ok := services["discoverd"]; ok {
			processor.discoverd = fmt.Sprintf("%s:%d", d.InternalIP, d.TCPPorts[0])
			var disc *discoverd.Client
			err = Attempts.Run(func() (err error) {
				disc, err = discoverd.NewClientWithAddr(processor.discoverd)
				return
			})
			if err != nil {
				log.Fatal(err)
			}
		}
	}

	if processor.discoverd == "" && *externalAddr != "" {
		processor.discoverd = *externalAddr + ":1111"
	}
	// HACK: use env as global for discoverd connection in sampic
	os.Setenv("DISCOVERD", processor.discoverd)
	if disc == nil {
		disc, err = discoverd.NewClientWithAddr(processor.discoverd)
		if err != nil {
			log.Fatal(err)
		}
	}
	sampiStandby, err := disc.RegisterAndStandby("flynn-host", *externalAddr+":1113", map[string]string{"id": *hostID})
	if err != nil {
		log.Fatal(err)
	}

	// Check if we are the leader so that we can use the cluster functions directly
	sampiCluster := sampi.NewCluster(sampi.NewState())
	select {
	case <-sampiStandby:
		g.Log(grohl.Data{"at": "sampi_leader"})
		rpc.Register(sampiCluster)
	case <-time.After(5 * time.Millisecond):
		go func() {
			<-sampiStandby
			g.Log(grohl.Data{"at": "sampi_leader"})
			rpc.Register(sampiCluster)
		}()
	}
	cluster, err := cluster.NewClientWithSelf(*hostID, NewLocalClient(*hostID, sampiCluster))
	if err != nil {
		log.Fatal(err)
	}

	g.Log(grohl.Data{"at": "sampi_connected"})

	events := make(chan host.Event)
	state.AddListener("all", events)
	go syncScheduler(cluster, events)

	h := &host.Host{}
	if *configFile != "" {
		h, err = openConfig(*configFile)
		if err != nil {
			log.Fatal(err)
		}
	}
	if h.Attributes == nil {
		h.Attributes = make(map[string]string)
	}
	for k, v := range attributes {
		h.Attributes[k] = v
	}
	h.ID = *hostID

	for {
		newLeader := cluster.NewLeaderSignal()

		h.Jobs = state.ClusterJobs()
		jobs := make(chan *host.Job)
		hostErr := cluster.RegisterHost(h, jobs)
		g.Log(grohl.Data{"at": "host_registered"})
		processor.Process(ports, jobs)
		g.Log(grohl.Data{"at": "sampi_disconnected", "err": *hostErr})

		<-newLeader
	}
}

type jobProcessor struct {
	externalAddr string
	bindAddr     string
	discoverd    string
	docker       interface {
		CreateContainer(*docker.Config) (*docker.Container, error)
		PullImage(docker.PullImageOptions, io.Writer) error
		StartContainer(string, *docker.HostConfig) error
		InspectContainer(string) (*docker.Container, error)
	}
	state *State
}

func killExistingContainers(dc *docker.Client) error {
	g := grohl.NewContext(grohl.Data{"fn": "kill_existing"})
	g.Log(grohl.Data{"at": "start"})
	containers, err := dc.ListContainers(docker.ListContainersOptions{})
	if err != nil {
		g.Log(grohl.Data{"at": "list", "status": "error", "err": err})
		return err
	}
outer:
	for _, c := range containers {
		for _, name := range c.Names {
			if strings.HasPrefix(name, "/flynn-") {
				g.Log(grohl.Data{"at": "kill", "container.id": c.ID, "container.name": name})
				if err := dc.KillContainer(c.ID); err != nil {
					g.Log(grohl.Data{"at": "kill", "container.id": c.ID, "container.name": name, "status": "error", "err": err})
				}
				continue outer
			}
		}
	}
	g.Log(grohl.Data{"at": "finish"})
	return nil
}

func (p *jobProcessor) Process(ports <-chan int, jobs chan *host.Job) {
	for job := range jobs {
		p.processJob(ports, job)
	}
}

func (p *jobProcessor) processJob(ports <-chan int, job *host.Job) (*docker.Container, error) {
	g := grohl.NewContext(grohl.Data{"fn": "process_job", "job.id": job.ID})
	g.Log(grohl.Data{"at": "start", "job.image": job.Config.Image, "job.cmd": job.Config.Cmd, "job.entrypoint": job.Config.Entrypoint})

	if job.HostConfig == nil {
		job.HostConfig = &docker.HostConfig{
			PortBindings: make(map[string][]docker.PortBinding, job.TCPPorts),
		}
	}
	if job.Config.ExposedPorts == nil {
		job.Config.ExposedPorts = make(map[string]struct{}, job.TCPPorts)
	}
	for i := 0; i < job.TCPPorts; i++ {
		port := strconv.Itoa(<-ports)
		if i == 0 {
			job.Config.Env = append(job.Config.Env, "PORT="+port)
		}
		job.Config.Env = append(job.Config.Env, fmt.Sprintf("PORT_%d=%s", i, port))
		job.Config.ExposedPorts[port+"/tcp"] = struct{}{}
		job.HostConfig.PortBindings[port+"/tcp"] = []docker.PortBinding{{HostPort: port, HostIp: p.bindAddr}}
	}

	job.Config.AttachStdout = true
	job.Config.AttachStderr = true
	if strings.HasPrefix(job.ID, "flynn-") {
		job.Config.Name = job.ID
	} else {
		job.Config.Name = "flynn-" + job.ID
	}
	if p.externalAddr != "" {
		job.Config.Env = appendUnique(job.Config.Env, "EXTERNAL_IP="+p.externalAddr, "SD_HOST="+p.externalAddr, "DISCOVERD="+p.discoverd)
	}

	p.state.AddJob(job)
	g.Log(grohl.Data{"at": "create_container"})
	container, err := p.docker.CreateContainer(job.Config)
	if err == docker.ErrNoSuchImage {
		g.Log(grohl.Data{"at": "pull_image"})
		err = p.docker.PullImage(docker.PullImageOptions{Repository: job.Config.Image}, os.Stdout)
		if err != nil {
			g.Log(grohl.Data{"at": "pull_image", "status": "error", "err": err})
			p.state.SetStatusFailed(job.ID, err)
			return nil, err
		}
		container, err = p.docker.CreateContainer(job.Config)
	}
	if err != nil {
		g.Log(grohl.Data{"at": "create_container", "status": "error", "err": err})
		p.state.SetStatusFailed(job.ID, err)
		return nil, err
	}
	p.state.SetContainerID(job.ID, container.ID)
	p.state.WaitAttach(job.ID)
	g.Log(grohl.Data{"at": "start_container"})
	if err := p.docker.StartContainer(container.ID, job.HostConfig); err != nil {
		g.Log(grohl.Data{"at": "start_container", "status": "error", "err": err})
		p.state.SetStatusFailed(job.ID, err)
		return nil, err
	}
	container, err = p.docker.InspectContainer(container.ID)
	if err != nil {
		g.Log(grohl.Data{"at": "inspect_container", "status": "error", "err": err})
		p.state.SetStatusFailed(job.ID, err)
		return nil, err
	}
	p.state.SetStatusRunning(job.ID, container.Volumes)
	g.Log(grohl.Data{"at": "finish"})
	return container, nil
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

type sampiClient interface {
	ConnectHost(*host.Host, chan *host.Job) *error
	RemoveJobs([]string) error
}

type sampiSyncClient interface {
	RemoveJobs([]string) error
}

func syncScheduler(scheduler sampiSyncClient, events <-chan host.Event) {
	for event := range events {
		if event.Event != "stop" {
			continue
		}
		grohl.Log(grohl.Data{"fn": "scheduler_event", "at": "remove_job", "job.id": event.JobID})
		if err := scheduler.RemoveJobs([]string{event.JobID}); err != nil {
			grohl.Log(grohl.Data{"fn": "scheduler_event", "at": "remove_job", "status": "error", "err": err, "job.id": event.JobID})
		}
	}
}

type dockerStreamClient interface {
	Events() (*docker.EventStream, error)
	InspectContainer(string) (*docker.Container, error)
}

func streamEvents(client dockerStreamClient, state *State) {
	stream, err := client.Events()
	if err != nil {
		log.Fatal(err)
	}
	for event := range stream.Events {
		if event.Status != "die" {
			continue
		}
		container, err := client.InspectContainer(event.ID)
		if err != nil {
			log.Println("inspect container", event.ID, "error:", err)
			// TODO: set job status anyway?
			continue
		}
		state.SetStatusDone(event.ID, container.State.ExitCode)
	}
}

// TODO: fix this, horribly broken

func allocatePorts(ports chan<- int, startPort, endPort int) {
	for i := startPort; i < endPort; i++ {
		ports <- i
	}
	// TODO: handle wrap-around
}
