// cni-nodad is a CNI meta-plugin that disables IPv6 DAD in the container
// network namespace before delegating to the actual CNI plugin (e.g. macvlan).
//
// It works by setting net.ipv6.conf.all.dad_transmits=0 and
// net.ipv6.conf.all.accept_dad=0 inside the target network namespace BEFORE
// the delegate plugin creates interfaces and assigns addresses. This ensures
// the kernel never performs DAD on any interface in that namespace.
//
// Usage in a NetworkAttachmentDefinition:
//
//	{
//	  "cniVersion": "0.3.1",
//	  "type": "cni-nodad",
//	  "delegate": {
//	    "type": "macvlan",
//	    "master": "vlan.1005",
//	    "mode": "bridge",
//	    "ipam": { ... }
//	  }
//	}
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/containernetworking/cni/pkg/invoke"
	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	"github.com/containernetworking/cni/pkg/version"
	"github.com/vishvananda/netns"
)

const sysctlBasePath = "/proc/sys"

// NetConf is the CNI network configuration for cni-nodad.
type NetConf struct {
	types.NetConf
	Delegate json.RawMessage `json:"delegate"`
}

func init() {
	// CNI plugins must lock the OS thread to ensure the network namespace
	// switch affects the correct goroutine.
	runtime.LockOSThread()
}

// setSysctl writes a value to a sysctl path inside the current namespace.
func setSysctl(name, value string) error {
	path := filepath.Join(sysctlBasePath, filepath.FromSlash(name))
	return os.WriteFile(path, []byte(value), 0o644)
}

// disableDADInNetns enters the given network namespace and sets the sysctls
// that suppress IPv6 DAD, then returns to the original namespace.
func disableDADInNetns(nsPath string) error {
	// Save current namespace.
	curNs, err := netns.Get()
	if err != nil {
		return fmt.Errorf("failed to get current netns: %w", err)
	}
	defer curNs.Close()

	// Open target namespace.
	targetNs, err := netns.GetFromPath(nsPath)
	if err != nil {
		return fmt.Errorf("failed to open netns %s: %w", nsPath, err)
	}
	defer targetNs.Close()

	// Switch to target namespace.
	if err := netns.Set(targetNs); err != nil {
		return fmt.Errorf("failed to enter netns %s: %w", nsPath, err)
	}

	// Set sysctls in the target namespace.
	sysctls := map[string]string{
		"net/ipv6/conf/all/dad_transmits": "0",
		"net/ipv6/conf/all/accept_dad":    "0",
	}
	for k, v := range sysctls {
		if err := setSysctl(k, v); err != nil {
			// Restore original namespace before returning error.
			_ = netns.Set(curNs)
			return fmt.Errorf("failed to set sysctl %s=%s: %w", k, v, err)
		}
	}

	// Return to original namespace.
	if err := netns.Set(curNs); err != nil {
		return fmt.Errorf("failed to restore original netns: %w", err)
	}

	return nil
}

func cmdAdd(args *skel.CmdArgs) error {
	conf := &NetConf{}
	if err := json.Unmarshal(args.StdinData, conf); err != nil {
		return fmt.Errorf("failed to parse config: %w", err)
	}

	// Disable DAD in the target network namespace before delegating.
	if err := disableDADInNetns(args.Netns); err != nil {
		return fmt.Errorf("failed to disable DAD: %w", err)
	}

	// Prepare the delegate config: inject cniVersion and name.
	delegateConf := make(map[string]interface{})
	if err := json.Unmarshal(conf.Delegate, &delegateConf); err != nil {
		return fmt.Errorf("failed to parse delegate config: %w", err)
	}
	delegateConf["cniVersion"] = conf.CNIVersion
	delegateConf["name"] = conf.Name

	delegateBytes, err := json.Marshal(delegateConf)
	if err != nil {
		return fmt.Errorf("failed to marshal delegate config: %w", err)
	}

	// Find and invoke the delegate plugin.
	delegateType, ok := delegateConf["type"].(string)
	if !ok || delegateType == "" {
		return fmt.Errorf("delegate config missing 'type' field")
	}

	paths := filepath.SplitList(os.Getenv("CNI_PATH"))
	pluginPath, err := invoke.FindInPath(delegateType, paths)
	if err != nil {
		return fmt.Errorf("failed to find delegate plugin %q: %w", delegateType, err)
	}

	result, err := invoke.ExecPluginWithResult(
		context.TODO(),
		pluginPath,
		delegateBytes,
		invoke.ArgsFromEnv(),
		nil,
	)
	if err != nil {
		return fmt.Errorf("delegate plugin %q failed: %w", delegateType, err)
	}

	return result.Print()
}

func cmdDel(args *skel.CmdArgs) error {
	conf := &NetConf{}
	if err := json.Unmarshal(args.StdinData, conf); err != nil {
		return fmt.Errorf("failed to parse config: %w", err)
	}

	delegateConf := make(map[string]interface{})
	if err := json.Unmarshal(conf.Delegate, &delegateConf); err != nil {
		return fmt.Errorf("failed to parse delegate config: %w", err)
	}
	delegateConf["cniVersion"] = conf.CNIVersion
	delegateConf["name"] = conf.Name

	delegateBytes, err := json.Marshal(delegateConf)
	if err != nil {
		return fmt.Errorf("failed to marshal delegate config: %w", err)
	}

	delegateType, ok := delegateConf["type"].(string)
	if !ok || delegateType == "" {
		return fmt.Errorf("delegate config missing 'type' field")
	}

	paths := filepath.SplitList(os.Getenv("CNI_PATH"))
	pluginPath, err := invoke.FindInPath(delegateType, paths)
	if err != nil {
		return fmt.Errorf("failed to find delegate plugin %q: %w", delegateType, err)
	}

	return invoke.ExecPluginWithoutResult(
		context.TODO(),
		pluginPath,
		delegateBytes,
		invoke.ArgsFromEnv(),
		nil,
	)
}

func cmdCheck(args *skel.CmdArgs) error {
	// Nothing to check — DAD disable is a fire-and-forget operation.
	return nil
}

func main() {
	skel.PluginMainFuncs(skel.CNIFuncs{
		Add:   cmdAdd,
		Del:   cmdDel,
		Check: cmdCheck,
	}, version.All, "cni-nodad: disable IPv6 DAD before delegating to another CNI plugin")
}
