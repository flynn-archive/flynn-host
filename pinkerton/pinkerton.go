package pinkerton

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os/exec"
)

type LayerPullInfo struct {
	ID     string
	Status string
}

func Pull(url string) ([]LayerPullInfo, error) {
	var layers []LayerPullInfo
	cmd := exec.Command("pinkerton", "pull", "--json", url)
	stdout, _ := cmd.StdoutPipe()
	cmd.Start()
	j := json.NewDecoder(stdout)
	for {
		var l LayerPullInfo
		if err := j.Decode(&l); err != nil {
			if err == io.EOF {
				break
			}
			go cmd.Wait()
			return nil, err
		}
		layers = append(layers, l)
	}
	return layers, cmd.Wait()
}

func Checkout(id, image string) (string, error) {
	cmd := exec.Command("pinkerton", "checkout", id, image)
	path, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(bytes.TrimSpace(path)), nil
}

func Cleanup(id string) error {
	return exec.Command("pinkerton", "cleanup", id).Run()
}

var ErrNoImageID = errors.New("pinkerton: missing image id")

func ImageID(s string) (string, error) {
	u, err := url.Parse(s)
	if err != nil {
		return "", err
	}
	q := u.Query()
	id := q.Get("id")
	if id == "" {
		return "", ErrNoImageID
	}
	return id, nil
}
