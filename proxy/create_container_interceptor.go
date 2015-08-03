package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"

	"github.com/fsouza/go-dockerclient"
	. "github.com/weaveworks/weave/common"
	"github.com/weaveworks/weave/nameserver"
)

const MaxDockerHostname = 64

var (
	ErrNoCommandSpecified = errors.New("No command specified")
)

type createContainerInterceptor struct{ proxy *Proxy }

type createContainerRequestBody struct {
	*docker.Config
	HostConfig *docker.HostConfig `json:"HostConfig,omitempty" yaml:"HostConfig,omitempty"`
	MacAddress string             `json:"MacAddress,omitempty" yaml:"MacAddress,omitempty"`
}

// ErrNoSuchImage replaces docker.NoSuchImage, which does not contain the image
// name, which in turn breaks docker clients post 1.7.0 since they expect the
// image name to be present in errors.
type ErrNoSuchImage struct {
	Name string
}

func (err *ErrNoSuchImage) Error() string {
	return "No such image: " + err.Name
}

func (i *createContainerInterceptor) InterceptRequest(r *http.Request) error {
	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		return err
	}
	r.Body.Close()

	container := createContainerRequestBody{}
	if err := json.Unmarshal(body, &container); err != nil {
		return err
	}

	if cidrs, err := i.proxy.weaveCIDRsFromConfig(container.Config, container.HostConfig); err != nil {
		Info.Printf("Ignoring container due to %s", err)
	} else {
		Info.Printf("Creating container with WEAVE_CIDR \"%s\"", strings.Join(cidrs, " "))
		if container.HostConfig == nil {
			container.HostConfig = &docker.HostConfig{}
		}
		if container.Config == nil {
			container.Config = &docker.Config{}
		}
		i.addWeaveWaitVolume(container.HostConfig)
		if err := i.setWeaveWaitEntrypoint(container.Config); err != nil {
			return err
		}
		if err := i.setWeaveDNS(&container, r); err != nil {
			return err
		}
	}

	newBody, err := json.Marshal(container)
	if err != nil {
		return err
	}
	r.Body = ioutil.NopCloser(bytes.NewReader(newBody))
	r.ContentLength = int64(len(newBody))

	return nil
}

func (i *createContainerInterceptor) addWeaveWaitVolume(hostConfig *docker.HostConfig) {
	var binds []string
	for _, bind := range hostConfig.Binds {
		s := strings.Split(bind, ":")
		if len(s) >= 2 && s[1] == "/w" {
			continue
		}
		binds = append(binds, bind)
	}
	hostConfig.Binds = append(binds, fmt.Sprintf("%s:/w:ro", i.proxy.weaveWaitVolume))
}

func (i *createContainerInterceptor) setWeaveWaitEntrypoint(container *docker.Config) error {
	if len(container.Entrypoint) == 0 {
		image, err := i.proxy.client.InspectImage(container.Image)
		if err == docker.ErrNoSuchImage {
			return &ErrNoSuchImage{container.Image}
		} else if err != nil {
			return err
		}

		if len(container.Cmd) == 0 {
			container.Cmd = image.Config.Cmd
		}

		if container.Entrypoint == nil {
			container.Entrypoint = image.Config.Entrypoint
		}
	}

	if len(container.Entrypoint) == 0 && len(container.Cmd) == 0 {
		return ErrNoCommandSpecified
	}

	if len(container.Entrypoint) == 0 || container.Entrypoint[0] != weaveWaitEntrypoint[0] {
		container.Entrypoint = append(weaveWaitEntrypoint, container.Entrypoint...)
	}

	return nil
}

func (i *createContainerInterceptor) setWeaveDNS(container *createContainerRequestBody, r *http.Request) error {
	if i.proxy.WithoutDNS {
		return nil
	}

	dnsDomain, dnsRunning := i.getDNSDomain()
	if !(dnsRunning || i.proxy.WithDNS) {
		return nil
	}

	container.HostConfig.DNS = append(container.HostConfig.DNS, i.proxy.dockerBridgeIP)

	name := r.URL.Query().Get("name")
	if container.Hostname == "" && name != "" {
		// Strip trailing period because it's unusual to see it used on the end of a host name
		trimmedDNSDomain := strings.TrimSuffix(dnsDomain, ".")
		if len(name)+1+len(trimmedDNSDomain) > MaxDockerHostname {
			Warning.Printf("Container name [%s] too long to be used as hostname", name)
		} else {
			container.Hostname = name
			container.Domainname = trimmedDNSDomain
		}
	}

	if len(container.HostConfig.DNSSearch) == 0 {
		if container.Hostname == "" {
			container.HostConfig.DNSSearch = []string{dnsDomain}
		} else {
			container.HostConfig.DNSSearch = []string{"."}
		}
	}

	return nil
}

func (i *createContainerInterceptor) getDNSDomain() (domain string, running bool) {
	domain = nameserver.DefaultLocalDomain
	dnsContainer, err := i.proxy.client.InspectContainer("weavedns")
	if err != nil ||
		dnsContainer.NetworkSettings == nil ||
		dnsContainer.NetworkSettings.IPAddress == "" {
		return
	}

	url := fmt.Sprintf("http://%s:%d/domain", dnsContainer.NetworkSettings.IPAddress, nameserver.DefaultHTTPPort)
	resp, err := http.Get(url)
	if err != nil || resp.StatusCode != http.StatusOK {
		return
	}
	defer resp.Body.Close()

	b, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return
	}

	return string(b), true
}

func (i *createContainerInterceptor) InterceptResponse(r *http.Response) error {
	return nil
}
