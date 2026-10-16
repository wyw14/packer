// Copyright IBM Corp. 2024, 2026
// SPDX-License-Identifier: BUSL-1.1

package packer

import (
	"crypto/sha256"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"

	packersdk "github.com/hashicorp/packer-plugin-sdk/packer"
	pluginsdk "github.com/hashicorp/packer-plugin-sdk/plugin"
	plugingetter "github.com/hashicorp/packer/packer/plugin-getter"
)

// PluginConfig helps load and use packer plugins
type PluginConfig struct {
	PluginDirectory string
	PluginMinPort   int
	PluginMaxPort   int
	Builders        BuilderSet
	Provisioners    ProvisionerSet
	PostProcessors  PostProcessorSet
	DataSources     DatasourceSet
	ReleasesOnly    bool
	// UseProtobuf is set if all the plugin candidates support protobuf, and
	// the user has not forced usage of gob for serialisation.
	UseProtobuf bool

	// Protocols selects the wire protocol feature version to use per caller
	// (plugin). It is normally initialised from the environment when Packer
	// starts; when nil it is lazily initialised on first use. Per-caller
	// decisions are frozen on each plugin client, so rollout changes and
	// rollbacks never affect already started plugin subprocesses.
	Protocols *ProtocolRollout
}

// protocolRollout returns the configured rollout, lazily initialising a
// default one (from the environment) for PluginConfig values created without
// one, such as in tests.
func (c *PluginConfig) protocolRollout() *ProtocolRollout {
	if c.Protocols == nil {
		c.Protocols = NewProtocolRolloutFromEnv()
	}
	return c.Protocols
}

// UseProtobufFor reports the wire protocol feature version currently selected
// for caller. The returned value is what a plugin client created now would
// freeze.
func (c *PluginConfig) UseProtobufFor(caller string) bool {
	return c.protocolRollout().UseProtobuf(caller)
}

// PACKERSPACE is used to represent the spaces that separate args for a command
// without being confused with spaces in the path to the command itself.
const PACKERSPACE = "-PACKERSPACE-"

var extractPluginBasename = regexp.MustCompile("^packer-plugin-([^_]+)")

// Discover discovers the latest installed version of each installed plugin.
//
// Search the directory of the executable, then the plugins directory, and
// finally the CWD, in that order. Any conflicts will overwrite previously
// found plugins, in that order.
// Hence, the priority order is the reverse of the search order - i.e., the
// CWD has the highest priority.
func (c *PluginConfig) Discover() error {
	if c.Builders == nil {
		c.Builders = MapOfBuilder{}
	}
	if c.Provisioners == nil {
		c.Provisioners = MapOfProvisioner{}
	}
	if c.PostProcessors == nil {
		c.PostProcessors = MapOfPostProcessor{}
	}
	if c.DataSources == nil {
		c.DataSources = MapOfDatasource{}
	}

	// If we are already inside a plugin process we should not need to
	// discover anything.
	if os.Getenv(pluginsdk.MagicCookieKey) == pluginsdk.MagicCookieValue {
		return nil
	}

	// Initialise the per-caller protocol rollout before any plugin gets
	// registered, so component factories can resolve their feature version.
	c.protocolRollout()

	if c.PluginDirectory == "" {
		c.PluginDirectory, _ = PluginFolder()
	}

	ext := ""
	if runtime.GOOS == "windows" {
		ext = ".exe"
	}

	installations, err := plugingetter.Requirement{}.ListInstallations(plugingetter.ListInstallationsOptions{
		PluginDirectory: c.PluginDirectory,
		BinaryInstallationOptions: plugingetter.BinaryInstallationOptions{
			OS:              runtime.GOOS,
			ARCH:            runtime.GOARCH,
			Ext:             ext,
			APIVersionMajor: pluginsdk.APIVersionMajor,
			APIVersionMinor: pluginsdk.APIVersionMinor,
			Checksummers: []plugingetter.Checksummer{
				{Type: "sha256", Hash: sha256.New()},
			},
			ReleasesOnly: c.ReleasesOnly,
		},
	})
	if err != nil {
		return err
	}

	// Map of plugin basename to executable
	//
	// We'll use that later to register the components for each plugin
	pluginMap := map[string]string{}
	for _, install := range installations {
		pluginBasename := filepath.Base(install.BinaryPath)
		matches := extractPluginBasename.FindStringSubmatch(pluginBasename)
		if len(matches) != 2 {
			log.Printf("[INFO] - plugin %q could not have its name matched, ignoring", pluginBasename)
			continue
		}

		pluginName := matches[1]
		pluginMap[pluginName] = install.BinaryPath
	}

	for name, path := range pluginMap {
		err := c.DiscoverMultiPlugin(name, path)
		if err != nil {
			return err
		}
	}

	return nil
}

const ForceGobEnvvar = "PACKER_FORCE_GOB"

// PackerUseProto records the default feature version for plugin
// communication: protobuf is on by default.
//
// Deprecated: protocol selection is owned by the per-caller ProtocolRollout
// held in PluginConfig.Protocols. This variable is only kept for
// compatibility with external importers and must not be mutated. A gob-only
// caller now degrades to gob on its own without downgrading the other
// callers, and PACKER_FORCE_GOB is honoured through the rollout.
var PackerUseProto = true

// StartPluginClient resolves the wire protocol feature version selected for
// caller, freezes that decision on a new plugin client, and prepares the
// command line for the plugin binary at path:
//
//	prefix [--protobuf] suffix
//
// --protobuf is only inserted when caller gets the protobuf feature version.
// The returned client is not started yet, but its protocol decision is
// immutable: later ProtocolRollout.Commit or Rollback changes only affect
// clients created afterwards, never in-flight requests against this one.
func (c *PluginConfig) StartPluginClient(caller, path string, prefix, suffix []string) *PluginClient {
	useProto := c.protocolRollout().UseProtobuf(caller)

	args := make([]string, 0, len(prefix)+len(suffix)+1)
	args = append(args, prefix...)
	if useProto {
		args = append(args, "--protobuf")
	}
	args = append(args, suffix...)

	client := c.Client(path, args...)
	// Freeze the decision so the RPC client and the plugin subprocess args
	// stay consistent for the whole lifetime of this client.
	client.config.UseProto = useProto
	return client
}

// DiscoverMultiPlugin takes the description from a multi-component plugin
// binary and makes the plugins available to use in Packer. Each plugin found in the
// binary will be addressable using `${pluginName}-${builderName}` for example.
// pluginName could be manually set. It usually is a cloud name like amazon.
// pluginName can be extrapolated from the filename of the binary; so
// if the "packer-plugin-amazon" binary had an "ebs" builder one could use
// the "amazon-ebs" builder.
func (c *PluginConfig) DiscoverMultiPlugin(pluginName, pluginPath string) error {
	desc, err := plugingetter.GetPluginDescription(pluginPath)
	if err != nil {
		return fmt.Errorf("failed to get plugin description from executable %q: %s", pluginPath, err)
	}

	return c.registerMultiPlugin(pluginName, pluginPath, desc)
}

// registerMultiPlugin registers the components advertised in desc for the
// plugin binary at pluginPath. It is split from DiscoverMultiPlugin so the
// protocol rollout can be exercised with synthetic descriptions without
// having to exec a plugin binary.
func (c *PluginConfig) registerMultiPlugin(pluginName, pluginPath string, desc pluginsdk.SetDescription) error {
	c.protocolRollout()

	// Record the caller's capability and, for an old client that only
	// supports gob, degrade only this caller to the stable gob response.
	// Other callers are unaffected.
	supported := protocolFromDescription(desc.ProtocolVersion)
	c.Protocols.SetSupport(pluginName, supported)
	if supported == WireProtocolGob {
		log.Printf("[INFO] plugin %q does not support protobuf (reported protocol_version %q); using the stable gob wire protocol for this plugin", pluginName, desc.ProtocolVersion)
	}

	pluginPrefix := pluginName + "-"
	pluginDetails := PluginDetails{
		Name:        pluginName,
		Description: desc,
		PluginPath:  pluginPath,
	}

	for _, builderName := range desc.Builders {
		builderName := builderName // copy to avoid pointer overwrite issue
		key := pluginPrefix + builderName
		if builderName == pluginsdk.DEFAULT_NAME {
			key = pluginName
		}
		if c.Builders.Has(key) {
			continue
		}

		c.Builders.Set(key, func() (packersdk.Builder, error) {
			return c.StartPluginClient(pluginName, pluginPath,
				[]string{"start", "builder"}, []string{builderName}).Builder()
		})
		GlobalPluginsDetailsStore.SetBuilder(key, pluginDetails)
	}

	if len(desc.Builders) > 0 {
		log.Printf("[INFO] found external %v builders from %s plugin", desc.Builders, pluginName)
	}

	for _, postProcessorName := range desc.PostProcessors {
		postProcessorName := postProcessorName // copy to avoid pointer overwrite issue
		key := pluginPrefix + postProcessorName
		if postProcessorName == pluginsdk.DEFAULT_NAME {
			key = pluginName
		}
		if c.PostProcessors.Has(key) {
			continue
		}

		c.PostProcessors.Set(key, func() (packersdk.PostProcessor, error) {
			return c.StartPluginClient(pluginName, pluginPath,
				[]string{"start", "post-processor"}, []string{postProcessorName}).PostProcessor()
		})
		GlobalPluginsDetailsStore.SetPostProcessor(key, pluginDetails)
	}

	if len(desc.PostProcessors) > 0 {
		log.Printf("[INFO] found external %v post-processors from %s plugin", desc.PostProcessors, pluginName)
	}

	for _, provisionerName := range desc.Provisioners {
		provisionerName := provisionerName // copy to avoid pointer overwrite issue
		key := pluginPrefix + provisionerName
		if provisionerName == pluginsdk.DEFAULT_NAME {
			key = pluginName
		}
		if c.Provisioners.Has(key) {
			continue
		}

		c.Provisioners.Set(key, func() (packersdk.Provisioner, error) {
			return c.StartPluginClient(pluginName, pluginPath,
				[]string{"start", "provisioner"}, []string{provisionerName}).Provisioner()
		})
		GlobalPluginsDetailsStore.SetProvisioner(key, pluginDetails)

	}
	if len(desc.Provisioners) > 0 {
		log.Printf("found external %v provisioner from %s plugin", desc.Provisioners, pluginName)
	}

	for _, datasourceName := range desc.Datasources {
		datasourceName := datasourceName // copy to avoid pointer overwrite issue
		key := pluginPrefix + datasourceName
		if datasourceName == pluginsdk.DEFAULT_NAME {
			key = pluginName
		}
		if c.DataSources.Has(key) {
			continue
		}

		c.DataSources.Set(key, func() (packersdk.Datasource, error) {
			return c.StartPluginClient(pluginName, pluginPath,
				[]string{"start", "datasource"}, []string{datasourceName}).Datasource()
		})
		GlobalPluginsDetailsStore.SetDataSource(key, pluginDetails)
	}
	if len(desc.Datasources) > 0 {
		log.Printf("found external %v datasource from %s plugin", desc.Datasources, pluginName)
	}

	return nil
}

func (c *PluginConfig) Client(path string, args ...string) *PluginClient {
	originalPath := path

	// Check for special case using `packer plugin PLUGIN`
	if strings.Contains(path, PACKERSPACE) {
		parts := strings.Split(path, PACKERSPACE)
		path = parts[0]
		args = parts[1:]
	}

	// First attempt to find the executable by consulting the PATH.
	path, err := exec.LookPath(path)
	if err != nil {
		// If that doesn't work, look for it in the same directory
		// as the `packer` executable (us).
		log.Printf("[INFO] exec.LookPath: %s : %v. Checking same directory as executable.", path, err)
		exePath, err := os.Executable()
		if err != nil {
			log.Printf("Couldn't get current exe path: %s", err)
		} else {
			log.Printf("Current exe path: %s", exePath)
			path = filepath.Join(filepath.Dir(exePath), filepath.Base(originalPath))
		}
	}

	// If everything failed, just use the original path and let the error
	// bubble through.
	if path == "" {
		path = originalPath
	}

	if strings.Contains(originalPath, PACKERSPACE) {
		log.Printf("[INFO] Starting internal plugin %s", args[len(args)-1])
	} else {
		log.Printf("[INFO] Starting external plugin %s %s", path, strings.Join(args, " "))
	}
	var config PluginClientConfig
	config.Cmd = exec.Command(path, args...)
	config.Managed = true
	config.MinPort = c.PluginMinPort
	config.MaxPort = c.PluginMaxPort
	return NewClient(&config)
}

type PluginComponentType string

const (
	PluginComponentBuilder       PluginComponentType = "builder"
	PluginComponentPostProcessor PluginComponentType = "post-processor"
	PluginComponentProvisioner   PluginComponentType = "provisioner"
	PluginComponentDataSource    PluginComponentType = "data-source"
)

type PluginDetails struct {
	Name        string
	Description pluginsdk.SetDescription
	PluginPath  string
}

type pluginsDetailsStorage struct {
	rwMutex sync.RWMutex
	data    map[string]PluginDetails
}

var GlobalPluginsDetailsStore = &pluginsDetailsStorage{
	data: make(map[string]PluginDetails),
}

func (pds *pluginsDetailsStorage) set(key string, plugin PluginDetails) {
	pds.rwMutex.Lock()
	defer pds.rwMutex.Unlock()
	pds.data[key] = plugin
}

func (pds *pluginsDetailsStorage) get(key string) (PluginDetails, bool) {
	pds.rwMutex.RLock()
	defer pds.rwMutex.RUnlock()
	plugin, exists := pds.data[key]
	return plugin, exists
}

func (pds *pluginsDetailsStorage) SetBuilder(name string, plugin PluginDetails) {
	key := fmt.Sprintf("%q-%q", PluginComponentBuilder, name)
	pds.set(key, plugin)
}

func (pds *pluginsDetailsStorage) GetBuilder(name string) (PluginDetails, bool) {
	key := fmt.Sprintf("%q-%q", PluginComponentBuilder, name)
	return pds.get(key)
}

func (pds *pluginsDetailsStorage) SetPostProcessor(name string, plugin PluginDetails) {
	key := fmt.Sprintf("%q-%q", PluginComponentPostProcessor, name)
	pds.set(key, plugin)
}

func (pds *pluginsDetailsStorage) GetPostProcessor(name string) (PluginDetails, bool) {
	key := fmt.Sprintf("%q-%q", PluginComponentPostProcessor, name)
	return pds.get(key)
}

func (pds *pluginsDetailsStorage) SetProvisioner(name string, plugin PluginDetails) {
	key := fmt.Sprintf("%q-%q", PluginComponentProvisioner, name)
	pds.set(key, plugin)
}

func (pds *pluginsDetailsStorage) GetProvisioner(name string) (PluginDetails, bool) {
	key := fmt.Sprintf("%q-%q", PluginComponentProvisioner, name)
	return pds.get(key)
}

func (pds *pluginsDetailsStorage) SetDataSource(name string, plugin PluginDetails) {
	key := fmt.Sprintf("%q-%q", PluginComponentDataSource, name)
	pds.set(key, plugin)
}

func (pds *pluginsDetailsStorage) GetDataSource(name string) (PluginDetails, bool) {
	key := fmt.Sprintf("%q-%q", PluginComponentDataSource, name)
	return pds.get(key)
}
