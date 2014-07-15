package main

import (
	"sync"
	"time"

	"github.com/flynn/flynn-host/types"
)

// TODO: prune old jobs?

type State struct {
	jobs map[string]*host.ActiveJob
	mtx  sync.RWMutex

	containers map[string]*host.ActiveJob              // docker container ID -> job
	listeners  map[string]map[chan host.Event]struct{} // job id -> listener list (ID "all" gets all events)
	listenMtx  sync.RWMutex
	attachers  map[string]chan struct{}
}

func NewState() *State {
	return &State{
		jobs:       make(map[string]*host.ActiveJob),
		containers: make(map[string]*host.ActiveJob),
		listeners:  make(map[string]map[chan host.Event]struct{}),
		attachers:  make(map[string]chan struct{}),
	}
}

func (s *State) AddJob(j *host.Job) {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	job := &host.ActiveJob{Job: j}
	s.jobs[j.ID] = job
	s.sendEvent(job, "create")
}

func (s *State) GetJob(id string) *host.ActiveJob {
	s.mtx.RLock()
	defer s.mtx.RUnlock()
	job := s.jobs[id]
	if job == nil {
		return nil
	}
	jobCopy := *job
	return &jobCopy
}

func (s *State) Get() map[string]host.ActiveJob {
	s.mtx.RLock()
	defer s.mtx.RUnlock()
	res := make(map[string]host.ActiveJob, len(s.jobs))
	for k, v := range s.jobs {
		res[k] = *v
	}
	return res
}

func (s *State) ClusterJobs() []*host.Job {
	s.mtx.RLock()
	defer s.mtx.RUnlock()

	res := make([]*host.Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		res = append(res, j.Job)
	}
	return res
}

func (s *State) SetContainerID(jobID, containerID string) {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	s.jobs[jobID].ContainerID = containerID
	s.containers[containerID] = s.jobs[jobID]
}

func (s *State) SetStatusRunning(jobID string) {
	s.mtx.Lock()
	defer s.mtx.Unlock()

	job, ok := s.jobs[jobID]
	if !ok {
		return
	}

	job.StartedAt = time.Now().UTC()
	job.Status = host.StatusRunning
	s.sendEvent(job, "start")
}

func (s *State) SetStatusDone(containerID string, exitCode int) {
	s.mtx.Lock()
	defer s.mtx.Unlock()

	job, ok := s.containers[containerID]
	if !ok || job.Status == host.StatusDone || job.Status == host.StatusCrashed || job.Status == host.StatusFailed {
		return
	}
	job.EndedAt = time.Now().UTC()
	job.ExitCode = exitCode
	if exitCode == 0 {
		job.Status = host.StatusDone
	} else {
		job.Status = host.StatusCrashed
	}
	s.sendEvent(job, "stop")
}

func (s *State) SetStatusFailed(jobID string, err error) {
	s.mtx.Lock()
	defer s.mtx.Unlock()

	job, ok := s.jobs[jobID]
	if !ok || job.Status == host.StatusDone || job.Status == host.StatusCrashed || job.Status == host.StatusFailed {
		return
	}
	job.Status = host.StatusFailed
	job.EndedAt = time.Now().UTC()
	errStr := err.Error()
	job.Error = &errStr
	s.sendEvent(job, "error")
}

func (s *State) AddAttacher(jobID string, ch chan struct{}) *host.ActiveJob {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	if job, ok := s.jobs[jobID]; ok {
		jobCopy := *job
		return &jobCopy
	}
	s.attachers[jobID] = ch
	// TODO: error if attach already waiting
	return nil
}

func (s *State) RemoveAttacher(jobID string, ch chan struct{}) {
	s.mtx.Lock()
	delete(s.attachers, jobID)
	s.mtx.Unlock()
}

func (s *State) WaitAttach(jobID string) {
	s.mtx.Lock()
	ch, ok := s.attachers[jobID]
	s.mtx.Unlock()
	if !ok {
		return
	}
	// signal attach
	ch <- struct{}{}
	// wait for attach
	<-ch
}

func (s *State) AddListener(jobID string, ch chan host.Event) {
	s.listenMtx.Lock()
	if _, ok := s.listeners[jobID]; !ok {
		s.listeners[jobID] = make(map[chan host.Event]struct{})
	}
	s.listeners[jobID][ch] = struct{}{}
	s.listenMtx.Unlock()
}

func (s *State) RemoveListener(jobID string, ch chan host.Event) {
	s.listenMtx.Lock()
	delete(s.listeners[jobID], ch)
	if len(s.listeners[jobID]) == 0 {
		delete(s.listeners, jobID)
	}
	s.listenMtx.Unlock()
}

func (s *State) sendEvent(job *host.ActiveJob, event string) {
	j := *job
	go func() {
		s.listenMtx.RLock()
		defer s.listenMtx.RUnlock()
		e := host.Event{JobID: job.Job.ID, Job: &j, Event: event}
		for ch := range s.listeners["all"] {
			ch <- e
		}
		for ch := range s.listeners[job.Job.ID] {
			ch <- e
		}
	}()
}
