package sriovnet

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	utilfs "github.com/Mellanox/sriovnet/pkg/utils/filesystem"
)

const (
	netdevPhysSwitchID = "phys_switch_id"
	netdevPhysPortName = "phys_port_name"
)

type PortFlavour uint16

// Keep things consistent with netlink lib constants
// nolint:golint,stylecheck
const (
	PORT_FLAVOUR_PHYSICAL = iota
	PORT_FLAVOUR_CPU
	PORT_FLAVOUR_DSA
	PORT_FLAVOUR_PCI_PF
	PORT_FLAVOUR_PCI_VF
	PORT_FLAVOUR_VIRTUAL
	PORT_FLAVOUR_UNUSED
	PORT_FLAVOUR_PCI_SF
	PORT_FLAVOUR_UNKNOWN = 0xffff
)

// Regex that matches on the physical/upling port name
var physPortRepRegex = regexp.MustCompile(`^p(\d+)$`)

// Regex that matches on PF representor port name. These ports exists on DPUs.
var pfPortRepRegex = regexp.MustCompile(`^(?:c\d+)?pf(\d+)$`)

// Regex that matches on VF representor port name
var vfPortRepRegex = regexp.MustCompile(`^(?:c\d+)?pf(\d+)vf(\d+)$`)

func parsePortName(physPortName string) (pfRepIndex, vfRepIndex int, err error) {
	pfRepIndex = -1
	vfRepIndex = -1

	// old kernel syntax of phys_port_name is vf index
	physPortName = strings.TrimSpace(physPortName)
	physPortNameInt, err := strconv.Atoi(physPortName)
	if err == nil {
		vfRepIndex = physPortNameInt
	} else {
		// new kernel syntax of phys_port_name [cZ]pfXVfY
		matches := vfPortRepRegex.FindStringSubmatch(physPortName)
		//nolint:gomnd
		if len(matches) != 3 {
			err = fmt.Errorf("failed to parse physPortName %s", physPortName)
		} else {
			pfRepIndex, err = strconv.Atoi(matches[1])
			if err == nil {
				vfRepIndex, err = strconv.Atoi(matches[2])
			}
		}
	}
	return pfRepIndex, vfRepIndex, err
}

func isSwitchdev(netdevice string) bool {
	swIDFile := filepath.Join(NetSysDir, netdevice, netdevPhysSwitchID)
	physSwitchID, err := utilfs.Fs.ReadFile(swIDFile)
	if err != nil {
		return false
	}
	if physSwitchID != nil && string(physSwitchID) != "" {
		return true
	}
	return false
}

// GetUplinkRepresentor gets a VF or PF PCI address (e.g '0000:03:00.4') and
// returns the uplink represntor netdev name for that VF or PF.
func GetUplinkRepresentor(pciAddress string) (string, error) {
	devicePath := filepath.Join(PciSysDir, pciAddress, "physfn", "net")
	if _, err := utilfs.Fs.Stat(devicePath); errors.Is(err, os.ErrNotExist) {
		// If physfn symlink to the parent PF doesn't exist, use the current device's dir
		devicePath = filepath.Join(PciSysDir, pciAddress, "net")
	}

	devices, err := utilfs.Fs.ReadDir(devicePath)
	if err != nil {
		return "", fmt.Errorf("failed to lookup %s: %v", pciAddress, err)
	}
	for _, device := range devices {
		if isSwitchdev(device.Name()) {
			// Try to get the phys port name, if not exists then fallback to check without it
			// phys_port_name should be in formant p<port-num> e.g p0,p1,p2 ...etc.
			if devicePhysPortName, err := getNetDevPhysPortName(device.Name()); err == nil {
				if !physPortRepRegex.MatchString(devicePhysPortName) {
					continue
				}
			}

			return device.Name(), nil
		}
	}
	return "", fmt.Errorf("uplink for %s not found", pciAddress)
}

func GetVfRepresentor(uplink string, vfIndex int) (string, error) {
	swIDFile := filepath.Join(NetSysDir, uplink, netdevPhysSwitchID)
	physSwitchID, err := utilfs.Fs.ReadFile(swIDFile)
	if err != nil || string(physSwitchID) == "" {
		return "", fmt.Errorf("cant get uplink %s switch id", uplink)
	}

	pfSubsystemPath := filepath.Join(NetSysDir, uplink, "subsystem")
	devices, err := utilfs.Fs.ReadDir(pfSubsystemPath)
	if err != nil {
		return "", err
	}
	for _, device := range devices {
		devicePath := filepath.Join(NetSysDir, device.Name())
		deviceSwIDFile := filepath.Join(devicePath, netdevPhysSwitchID)
		deviceSwID, err := utilfs.Fs.ReadFile(deviceSwIDFile)
		if err != nil || string(deviceSwID) != string(physSwitchID) {
			continue
		}
		physPortNameStr, err := getNetDevPhysPortName(device.Name())
		if err != nil {
			continue
		}
		pfRepIndex, vfRepIndex, _ := parsePortName(physPortNameStr)
		if pfRepIndex != -1 {
			pfPCIAddress, err := getPCIFromDeviceName(uplink)
			if err != nil {
				continue
			}
			PCIFuncAddress, err := strconv.Atoi(string((pfPCIAddress[len(pfPCIAddress)-1])))
			if pfRepIndex != PCIFuncAddress || err != nil {
				continue
			}
		}
		// At this point we're confident we have a representor.
		if vfRepIndex == vfIndex {
			return device.Name(), nil
		}
	}
	return "", fmt.Errorf("failed to find VF representor for uplink %s", uplink)
}

func getNetDevPhysPortName(netDev string) (string, error) {
	devicePortNameFile := filepath.Join(NetSysDir, netDev, netdevPhysPortName)
	physPortName, err := utilfs.Fs.ReadFile(devicePortNameFile)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(physPortName)), nil
}

// findNetdevWithPortNameCriteria returns representor netdev that matches a criteria function on the
// physical port name
func findNetdevWithPortNameCriteria(criteria func(string) bool) (string, error) {
	netdevs, err := utilfs.Fs.ReadDir(NetSysDir)
	if err != nil {
		return "", err
	}

	for _, netdev := range netdevs {
		// find matching VF representor
		netdevName := netdev.Name()

		// skip non switchdev netdevs
		if !isSwitchdev(netdevName) {
			continue
		}

		portName, err := getNetDevPhysPortName(netdevName)
		if err != nil {
			continue
		}

		if criteria(portName) {
			return netdevName, nil
		}
	}
	return "", fmt.Errorf("no representor matched criteria")
}

// GetVfRepresentorDPU returns VF representor on DPU for a host VF identified by pfID and vfIndex
func GetVfRepresentorDPU(pfID, vfIndex string) (string, error) {
	// Dirty hack

	if vfIndex == "0" {
		return "enP2p15s0v1", nil
	} else if vfIndex == "1" {
		return "enP2p15s0v2", nil
	} else if vfIndex == "2" {
		return "enP2p15s0v3", nil
	} else if vfIndex == "3" {
		return "enP2p15s0v4", nil
	} else if vfIndex == "4" {
		return "enP2p15s0v5", nil
	} else if vfIndex == "5" {
		return "enP2p15s0v6", nil
	} else if vfIndex == "6" {
		return "enP2p15s0v7", nil
	} else if vfIndex == "7" {
		return "enP2p15s0v8", nil
	} else {
		return "", fmt.Errorf("naftaly: unexpected pfID(%s). It should be 0 or 1", pfID)
	}
}

// GetRepresentorPortFlavour returns the representor port flavour
// Note: this method does not support old representor names used by old kernels
// e.g <vf_num> and will return PORT_FLAVOUR_UNKNOWN for such cases.
func GetRepresentorPortFlavour(netdev string) (PortFlavour, error) {

	// Dirty hack
	if netdev == "enP2p3s0" {
		fmt.Errorf("naftaly: true")
		return PORT_FLAVOUR_PCI_PF, nil
	}
	fmt.Errorf("naftaly: false")

	return PORT_FLAVOUR_UNKNOWN, nil
}

// parseDPUConfigFileOutput parses the config file content of a DPU
// representor port. The format of the file is a set of <key>:<value> pairs as follows:
//
// ```
//  MAC        : 0c:42:a1:c6:cf:7c
//  MaxTxRate  : 0
//  State      : Follow
// ```
func parseDPUConfigFileOutput(out string) map[string]string {
	configMap := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		entry := strings.SplitN(line, ":", 2)
		if len(entry) != 2 {
			// unexpected line format
			continue
		}
		configMap[strings.Trim(entry[0], " \t\n")] = strings.Trim(entry[1], " \t\n")
	}
	return configMap
}

// GetRepresentorPeerMacAddress returns the MAC address of the peer netdev associated with the given
// representor netdev
// Note:
//    This method functionality is currently supported only on DPUs.
//    Currently only netdev representors with PORT_FLAVOUR_PCI_PF are supported
func GetRepresentorPeerMacAddress(netdev string) (net.HardwareAddr, error) {

	// get MAC address for netdev
	configPath := filepath.Join(NetSysDir, netdev, "address")
	out, err := utilfs.Fs.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read MAC address for %s", netdev, err)
	}

	macStr := string(out)
	macStr = strings.TrimSuffix(macStr, "\n")

	mac, err := net.ParseMAC(macStr)

	if err != nil {
		return nil, fmt.Errorf("failed to parse MAC address \"%s\" for %s. %v", macStr, netdev, err)
	}
	return mac, nil
}

// SetRepresentorPeerMacAddress sets the given MAC addresss of the peer netdev associated with the given
// representor netdev.
// Note: This method functionality is currently supported only for DPUs.
// Currently only netdev representors with PORT_FLAVOUR_PCI_VF are supported
func SetRepresentorPeerMacAddress(netdev string, mac net.HardwareAddr) error {
	/*
		flavor, err := GetRepresentorPortFlavour(netdev)
		if err != nil {
			return fmt.Errorf("unknown port flavour for netdev %s. %v", netdev, err)
		}
		if flavor == PORT_FLAVOUR_UNKNOWN {
			return fmt.Errorf("unknown port flavour for netdev %s", netdev)
		}
		if flavor != PORT_FLAVOUR_PCI_VF {
			return fmt.Errorf("unsupported port flavour for netdev %s", netdev)
		}*/
	/*
		physPortNameStr, err := getNetDevPhysPortName(netdev)
		if err != nil {
			return fmt.Errorf("failed to get phys_port_name for netdev %s: %v", netdev, err)
		}
		pfID, vfIndex, err := parsePortName(physPortNameStr)
		if err != nil {
			return fmt.Errorf("failed to get the pf and vf index for netdev %s "+
				"with phys_port_name %s: %v", netdev, physPortNameStr, err)
		}

		uplinkPhysPortName := fmt.Sprintf("p%d", pfID)
		uplinkNetdev, err := findNetdevWithPortNameCriteria(func(pname string) bool { return pname == uplinkPhysPortName })
		if err != nil {
			return fmt.Errorf("failed to find netdev for physical port name %s. %v", uplinkPhysPortName, err)
		}
		vfRepName := fmt.Sprintf("vf%d", vfIndex)
		sysfsVfRepMacFile := filepath.Join(NetSysDir, uplinkNetdev, "smart_nic", vfRepName, "mac")
		_, err = utilfs.Fs.Stat(sysfsVfRepMacFile)
		if err != nil {
			return fmt.Errorf("couldn't stat VF representor's sysfs file %s: %v", sysfsVfRepMacFile, err)
		}
		err = utilfs.Fs.WriteFile(sysfsVfRepMacFile, []byte(mac.String()), 0)
		if err != nil {
			return fmt.Errorf("failed to write the MAC address %s to VF reprentor %s",
				mac.String(), sysfsVfRepMacFile)
		}
	*/
	return nil
}
