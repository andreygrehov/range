//go:build darwin && cgo

package vm

import (
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/Code-Hex/vz/v3"
)

// Spec is the VM to boot.
type Spec struct {
	Kernel, Initrd string
	CPUs           uint
	Memory         uint64
	// Disks are nbd:// URLs, attached read-only in order as vda, vdb, ...
	Disks []string
	// Console receives the kernel's and init's output; nil discards it.
	Console *os.File
}

// Machine is a running VM.
type Machine struct {
	vm      *vz.VirtualMachine
	sockets *vz.VirtioSocketDevice
	stopped chan struct{}
	// keep holds every object the VM is built from. The vz package releases
	// the Objective-C object behind a Go value when the garbage collector
	// finalizes it; an attachment released while the VM runs makes Apple drop
	// the disk, and the guest hangs on its next read.
	keep []any
}

// Start boots a VM from spec.
func Start(spec Spec) (*Machine, error) {
	m := &Machine{stopped: make(chan struct{})}
	boot, err := vz.NewLinuxBootLoader(spec.Kernel,
		vz.WithCommandLine("console=hvc0 quiet loglevel=3 rdinit=/init"),
		vz.WithInitrd(spec.Initrd))
	if err != nil {
		return nil, fmt.Errorf("boot loader: %w", err)
	}
	config, err := vz.NewVirtualMachineConfiguration(boot, spec.CPUs, spec.Memory)
	if err != nil {
		return nil, fmt.Errorf("vm configuration: %w", err)
	}
	m.keep = append(m.keep, boot, config)

	console := spec.Console
	if console == nil {
		if console, err = os.OpenFile(os.DevNull, os.O_WRONLY, 0); err != nil {
			return nil, err
		}
	}
	input, err := os.Open(os.DevNull)
	if err != nil {
		return nil, err
	}
	serial, err := vz.NewFileHandleSerialPortAttachment(input, console)
	if err != nil {
		return nil, fmt.Errorf("console: %w", err)
	}
	port, err := vz.NewVirtioConsoleDeviceSerialPortConfiguration(serial)
	if err != nil {
		return nil, fmt.Errorf("console: %w", err)
	}
	config.SetSerialPortsVirtualMachineConfiguration([]*vz.VirtioConsoleDeviceSerialPortConfiguration{port})
	m.keep = append(m.keep, console, input, serial, port)

	var disks []vz.StorageDeviceConfiguration
	for _, url := range spec.Disks {
		attachment, err := vz.NewNetworkBlockDeviceStorageDeviceAttachment(url, 30*time.Second, true,
			vz.DiskSynchronizationModeNone)
		if err != nil {
			return nil, fmt.Errorf("disk %s: %w", url, err)
		}
		disk, err := vz.NewVirtioBlockDeviceConfiguration(attachment)
		if err != nil {
			return nil, fmt.Errorf("disk %s: %w", url, err)
		}
		disks = append(disks, disk)
		m.keep = append(m.keep, attachment, disk)
	}
	config.SetStorageDevicesVirtualMachineConfiguration(disks)

	nat, err := vz.NewNATNetworkDeviceAttachment()
	if err != nil {
		return nil, fmt.Errorf("network: %w", err)
	}
	network, err := vz.NewVirtioNetworkDeviceConfiguration(nat)
	if err != nil {
		return nil, fmt.Errorf("network: %w", err)
	}
	mac, err := vz.NewRandomLocallyAdministeredMACAddress()
	if err != nil {
		return nil, fmt.Errorf("network: %w", err)
	}
	network.SetMACAddress(mac)
	config.SetNetworkDevicesVirtualMachineConfiguration([]*vz.VirtioNetworkDeviceConfiguration{network})
	m.keep = append(m.keep, nat, network, mac)

	entropy, err := vz.NewVirtioEntropyDeviceConfiguration()
	if err != nil {
		return nil, fmt.Errorf("entropy: %w", err)
	}
	config.SetEntropyDevicesVirtualMachineConfiguration([]*vz.VirtioEntropyDeviceConfiguration{entropy})
	socket, err := vz.NewVirtioSocketDeviceConfiguration()
	if err != nil {
		return nil, fmt.Errorf("vsock: %w", err)
	}
	config.SetSocketDevicesVirtualMachineConfiguration([]vz.SocketDeviceConfiguration{socket})
	m.keep = append(m.keep, entropy, socket)

	if ok, err := config.Validate(); !ok || err != nil {
		return nil, fmt.Errorf("vm configuration: %w", err)
	}
	m.vm, err = vz.NewVirtualMachine(config)
	if err != nil {
		return nil, fmt.Errorf("create vm: %w", err)
	}
	devices := m.vm.SocketDevices()
	if len(devices) == 0 {
		return nil, errors.New("the vm has no vsock device")
	}
	m.sockets = devices[0]
	go func() {
		for state := range m.vm.StateChangedNotify() {
			if state == vz.VirtualMachineStateStopped || state == vz.VirtualMachineStateError {
				close(m.stopped)
				return
			}
		}
	}()
	if err := m.vm.Start(); err != nil {
		return nil, fmt.Errorf("start vm: %w", err)
	}
	return m, nil
}

// Listen accepts the guest's connections to a vsock port.
func (m *Machine) Listen(port uint32) (net.Listener, error) {
	return m.sockets.Listen(port)
}

// Stopped is closed once the VM has stopped.
func (m *Machine) Stopped() <-chan struct{} { return m.stopped }

// Stop ends the VM at once and waits for it.
func (m *Machine) Stop() {
	select {
	case <-m.stopped:
		return
	default:
	}
	if m.vm.CanStop() {
		m.vm.Stop()
	}
	select {
	case <-m.stopped:
	case <-time.After(5 * time.Second):
	}
}

// Close releases the VM. Call it after the VM has stopped.
func (m *Machine) Close() {
	m.keep = nil
}
