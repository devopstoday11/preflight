package versionchecker

import (
	"context"
	"fmt"
	"io/ioutil"
	"net/url"
	"os"
	"strings"
	"time"

	vcapi "github.com/jetstack/version-checker/pkg/api"
	vcclient "github.com/jetstack/version-checker/pkg/client"
	vcselfhosted "github.com/jetstack/version-checker/pkg/client/selfhosted"
	vcchecker "github.com/jetstack/version-checker/pkg/controller/checker"
	vcsearch "github.com/jetstack/version-checker/pkg/controller/search"
	vcversion "github.com/jetstack/version-checker/pkg/version"
	"github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/jetstack/preflight/pkg/datagatherer"
	"github.com/jetstack/preflight/pkg/datagatherer/k8s"
)

const (
	// these are the keys used to look the file paths up from the supplied
	// config
	gcrTokenKey = "token"

	acrUsernameKey     = "username"
	acrPasswordKey     = "password"
	acrRefreshTokenKey = "refresh_token"

	ecrAccessKeyIdKey     = "access_key_id"
	ecrSecretAccessKeyKey = "secret_access_key"
	ecrSessionTokenKey    = "session_token"

	dockerUsernameKey = "username"
	dockerPasswordKey = "password"
	dockerTokenKey    = "token"

	quayTokenKey = "token"

	selfhostedUsernameKey = "username"
	selfhostedPasswordKey = "password"
	selfhostedBearerKey   = "bearer"
	selfhostedHostKey     = "host"
)

// Config is the configuration for a VersionChecker DataGatherer.
type Config struct {
	// the version checker dg will also gather pods and so has the same options
	// as the dynamic datagatherer
	Dynamic                     k8s.ConfigDynamic
	VersionCheckerClientOptions vcclient.Options
	// Currently unused, but keeping to allow future config of VersionChecker
	VersionCheckerCheckerOptions vcapi.Options
}

// UnmarshalYAML unmarshals the ConfigDynamic resolving GroupVersionResource.
func (c *Config) UnmarshalYAML(unmarshal func(interface{}) error) error {
	aux := struct {
		Dynamic struct {
			KubeConfigPath    string   `yaml:"kubeconfig"`
			ExcludeNamespaces []string `yaml:"exclude-namespaces"`
			IncludeNamespaces []string `yaml:"include-namespaces"`
		} `yaml:"k8s"`
		Registries []struct {
			Kind   string            `yaml:"kind"`
			Params map[string]string `yaml:"params"`
		} `yaml:"registries"`
	}{}
	err := unmarshal(&aux)
	if err != nil {
		return fmt.Errorf("failed to unmarshal version checker config: %s", err)
	}

	c.Dynamic.KubeConfigPath = aux.Dynamic.KubeConfigPath
	c.Dynamic.ExcludeNamespaces = aux.Dynamic.ExcludeNamespaces
	c.Dynamic.IncludeNamespaces = aux.Dynamic.IncludeNamespaces
	// gvr must be pods for the version checker dg
	c.Dynamic.GroupVersionResource.Group = ""
	c.Dynamic.GroupVersionResource.Version = "v1"
	c.Dynamic.GroupVersionResource.Resource = "pods"

	c.VersionCheckerClientOptions.Selfhosted = map[string]*vcselfhosted.Options{}
	registryKindCounts := map[string]int{}
	for i, v := range aux.Registries {
		registryKindCounts[v.Kind]++
		switch v.Kind {
		case "gcr":
			data, err := loadKeysFromPaths([]string{gcrTokenKey}, v.Params)
			if err != nil {
				return fmt.Errorf("failed to load params for %s registry %d/%d: %s", v.Kind, i+1, len(aux.Registries), err)
			}

			c.VersionCheckerClientOptions.GCR.Token = data[gcrTokenKey]
		case "acr":
			data, err := loadKeysFromPaths([]string{acrUsernameKey, acrPasswordKey, acrRefreshTokenKey}, v.Params)
			if err != nil {
				return fmt.Errorf("failed to load params for %s registry %d/%d: %s", v.Kind, i+1, len(aux.Registries), err)
			}

			c.VersionCheckerClientOptions.ACR.Username = data[acrUsernameKey]
			c.VersionCheckerClientOptions.ACR.Password = data[acrPasswordKey]
			c.VersionCheckerClientOptions.ACR.RefreshToken = data[acrRefreshTokenKey]
		case "ecr":
			data, err := loadKeysFromPaths([]string{ecrAccessKeyIdKey, ecrSecretAccessKeyKey, ecrSessionTokenKey}, v.Params)
			if err != nil {
				return fmt.Errorf("failed to load params for %s registry %d/%d: %s", v.Kind, i+1, len(aux.Registries), err)
			}

			c.VersionCheckerClientOptions.ECR.AccessKeyID = data[ecrAccessKeyIdKey]
			c.VersionCheckerClientOptions.ECR.SecretAccessKey = data[ecrSecretAccessKeyKey]
			c.VersionCheckerClientOptions.ECR.SessionToken = data[ecrSessionTokenKey]
		case "docker":
			data, err := loadKeysFromPaths([]string{dockerUsernameKey, dockerPasswordKey, dockerTokenKey}, v.Params)
			if err != nil {
				return fmt.Errorf("failed to load params for %s registry %d/%d: %s", v.Kind, i+1, len(aux.Registries), err)
			}

			c.VersionCheckerClientOptions.Docker.Username = data[dockerUsernameKey]
			c.VersionCheckerClientOptions.Docker.Password = data[dockerPasswordKey]
			c.VersionCheckerClientOptions.Docker.Token = data[dockerTokenKey]
		case "quay":
			data, err := loadKeysFromPaths([]string{quayTokenKey}, v.Params)
			if err != nil {
				return fmt.Errorf("failed to load params for %s registry %d/%d: %s", v.Kind, i+1, len(aux.Registries), err)
			}

			c.VersionCheckerClientOptions.Quay.Token = data[quayTokenKey]
		case "selfhosted":
			// currently, version checker only supports multiple selfhosted registries
			data, err := loadKeysFromPaths([]string{selfhostedUsernameKey, selfhostedPasswordKey, selfhostedHostKey, selfhostedBearerKey}, v.Params)
			if err != nil {
				return fmt.Errorf("failed to load params for %s registry %d/%d: %s", v.Kind, i+1, len(aux.Registries), err)
			}

			opts := vcselfhosted.Options{
				Username: data[selfhostedUsernameKey],
				Password: data[selfhostedPasswordKey],
				Bearer:   data[selfhostedBearerKey],
				Host:     data[selfhostedHostKey],
			}

			if len(opts.Host) == 0 {
				return fmt.Errorf("failed to init selfhosted dg, host is required (registry %d/%d): %s", i+1, len(aux.Registries), err)
			}

			parsedURL, err := url.Parse(opts.Host)
			if err != nil {
				return fmt.Errorf("failed to parse host %s (registry %d/%d): %s", opts.Host, i+1, len(aux.Registries), err)
			}

			c.VersionCheckerClientOptions.Selfhosted[parsedURL.Host] = &opts
		default:
			return fmt.Errorf("registry %d/%d was an unknown kind (%s)", i+1, len(aux.Registries), v.Kind)
		}
	}

	// this is only needed while version checker only supports one registry of
	// each kind. Using an array of registries in the config allows us to
	// support many in future without changing the config format.
	for k, v := range registryKindCounts {
		if v > 1 && k != "selfhosted" {
			return fmt.Errorf("found %d registries of kind %s, only 1 is supported", v, k)
		}
	}

	return nil
}

func loadKeysFromPaths(keys []string, params map[string]string) (map[string]string, error) {
	requiredKeys := map[string]bool{}
	for _, v := range keys {
		requiredKeys[v] = false
	}

	loadedData := map[string]string{}
	for _, k := range keys {
		path := params[k]
		if path == "" {
			// don't try to load unset secrets. version-checker will fail if
			// config is missing
			continue
		}

		b, err := ioutil.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("failed to read file for %s at %s: %s", k, path, err)
		}
		loadedData[k] = strings.TrimSpace(string(b))
	}

	return loadedData, nil
}

// NewDataGatherer creates a new VersionChecker DataGatherer
func (c *Config) NewDataGatherer(ctx context.Context) (datagatherer.DataGatherer, error) {
	// create the k8s DataGatherer to use when collecting pods
	dynamicDg, err := c.Dynamic.NewDataGatherer(ctx)
	if err != nil {
		return nil, err
	}

	// configure version checker parameters
	vclog := logrus.New()
	vclog.SetOutput(os.Stdout)
	log := logrus.NewEntry(vclog)
	imageClient, err := vcclient.New(ctx, log, c.VersionCheckerClientOptions)
	if err != nil {
		return nil, fmt.Errorf("failed to setup version checker image registry clients: %s", err)
	}
	timeout := 30 * time.Minute // this is the default used in version checker
	search := vcsearch.New(
		log,
		timeout,
		vcversion.New(log, imageClient, timeout),
	)

	// dg wraps version checker and dynamic client to request pods
	return &DataGatherer{
		ctx:                   ctx,
		config:                c,
		dynamicDg:             dynamicDg,
		versionChecker:        vcchecker.New(search),
		versionCheckerLog:     log,
		versionCheckerOptions: c.VersionCheckerCheckerOptions,
	}, nil
}

// DataGatherer is a VersionChecker DataGatherer
type DataGatherer struct {
	ctx                   context.Context
	config                *Config
	dynamicDg             datagatherer.DataGatherer
	versionChecker        *vcchecker.Checker
	versionCheckerLog     *logrus.Entry
	versionCheckerOptions vcapi.Options
}

// PodResult wraps a pod and a version checker result for an image found in one
// of the containers for that pod. Exported so the backend can destructure
// json.
type PodResult struct {
	Pod     v1.Pod            `json:"pod"`
	Results []containerResult `json:"results"`
}

type containerResult struct {
	ContainerName   string            `json:"container_name"`
	InitContinainer bool              `json:"init_container"`
	Result          *vcchecker.Result `json:"result"`
}

// Fetch retrieves cluster information from GKE.
func (g *DataGatherer) Fetch() (interface{}, error) {
	rawPods, err := g.dynamicDg.Fetch()
	if err != nil {
		return nil, err
	}

	pods, ok := rawPods.(*unstructured.UnstructuredList)
	if !ok {
		return nil, fmt.Errorf("failed to parse pods loaded from DataGatherer")
	}

	var results []PodResult
	for _, v := range pods.Items {
		var pod v1.Pod
		err = runtime.DefaultUnstructuredConverter.FromUnstructured(v.Object, &pod)
		if err != nil {
			return nil, fmt.Errorf("failed to parse pod from unstructured data")
		}

		var allContainers []v1.Container
		var isInitContainer []bool
		for _, c := range pod.Spec.Containers {
			allContainers = append(allContainers, c)
			isInitContainer = append(isInitContainer, false)
		}
		for _, c := range pod.Spec.InitContainers {
			allContainers = append(allContainers, c)
			isInitContainer = append(isInitContainer, true)
		}

		var containerResults []containerResult
		for i, c := range allContainers {
			result, err := g.versionChecker.Container(g.ctx, g.versionCheckerLog, &pod, &c, &g.versionCheckerOptions)
			if err != nil {
				return nil, fmt.Errorf("failed to check image for container: %s", err)
			}

			containerResults = append(
				containerResults,
				containerResult{
					ContainerName:   c.Name,
					InitContinainer: isInitContainer[i],
					Result:          result,
				},
			)
		}

		results = append(results, PodResult{Pod: pod, Results: containerResults})
	}

	return results, nil
}
