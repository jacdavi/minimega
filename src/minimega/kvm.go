// Copyright (2012) Sandia Corporation.
// Under the terms of Contract DE-AC04-94AL85000 with Sandia Corporation,
// the U.S. Government retains certain rights in this software.

package main

import (
	"bridge"
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	log "minilog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"qmp"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"
	"vnc"
)

const (
	DEV_PER_BUS    = 32
	DEV_PER_VIRTIO = 30 // Max of 30 virtio ports/device (0 and 32 are reserved)

	DefaultKVMCPU = "host"
)

type KVMConfig struct {
	// Set the QEMU process to invoke. Relative paths are ok. When unspecified,
	// minimega uses "kvm" in the default path.
	//
	// Note: this configuration only applies to KVM-based VMs.
	QemuPath string

	// Attach a kernel image to a VM. If set, QEMU will boot from this image
	// instead of any disk image.
	//
	// Note: this configuration only applies to KVM-based VMs.
	KernelPath string

	// Attach an initrd image to a VM. Passed along with the kernel image at
	// boot time.
	//
	// Note: this configuration only applies to KVM-based VMs.
	InitrdPath string

	// Attach a cdrom to a VM. When using a cdrom, it will automatically be set
	// to be the boot device.
	//
	// Note: this configuration only applies to KVM-based VMs.
	CdromPath string

	// Assign a migration image, generated by a previously saved VM to boot
	// with. By default, images are read from the files directory as specified
	// with -filepath. This can be overriden by using an absolute path.
	// Migration images should be booted with a kernel/initrd, disk, or cdrom.
	// Use 'vm migrate' to generate migration images from running VMs.
	//
	// Note: this configuration only applies to KVM-based VMs.
	MigratePath string

	// Set the virtual CPU architecture.
	//
	// By default, set to 'host' which matches the host architecture. See 'kvm
	// -cpu help' for a list of architectures available for your version of
	// kvm.
	//
	// Note: this configuration only applies to KVM-based VMs.
	//
	// Default: "host"
	CPU string

	// Specify the serial ports that will be created for the VM to use. Serial
	// ports specified will be mapped to the VM's /dev/ttySX device, where X
	// refers to the connected unix socket on the host at
	// $minimega_runtime/<vm_id>/serialX.
	//
	// Examples:
	//
	// To display current serial ports:
	//   vm config serial
	//
	// To create three serial ports:
	//   vm config serial 3
	//
	// Note: Whereas modern versions of Windows support up to 256 COM ports,
	// Linux typically only supports up to four serial devices. To use more,
	// make sure to pass "8250.n_uarts = 4" to the guest Linux kernel at boot.
	// Replace 4 with another number.
	SerialPorts uint64

	// Specify the virtio-serial ports that will be created for the VM to use.
	// Virtio-serial ports specified will be mapped to the VM's
	// /dev/virtio-port/<portname> device, where <portname> refers to the
	// connected unix socket on the host at
	// $minimega_runtime/<vm_id>/virtio-serialX.
	//
	// Examples:
	//
	// To display current virtio-serial ports:
	//   vm config virtio-serial
	//
	// To create three virtio-serial ports:
	//   vm config virtio-serial 3
	VirtioPorts uint64

	// Add an append string to a kernel set with vm kernel. Setting vm append
	// without using vm kernel will result in an error.
	//
	// For example, to set a static IP for a linux VM:
	//
	// 	vm config append ip=10.0.0.5 gateway=10.0.0.1 netmask=255.255.255.0 dns=10.10.10.10
	//
	// Note: this configuration only applies to KVM-based VMs.
	Append []string

	// Attach one or more disks to a vm. Any disk image supported by QEMU is a
	// valid parameter. Disk images launched in snapshot mode may safely be
	// used for multiple VMs.
	//
	// Note: this configuration only applies to KVM-based VMs.
	DiskPaths []string

	// Add additional arguments to be passed to the QEMU instance. For example:
	//
	// 	vm config qemu-append -serial tcp:localhost:4001
	//
	// Note: this configuration only applies to KVM-based VMs.
	QemuAppend []string

	// QemuOverride for the VM, handler is not generated by vmconfiger.
	QemuOverride []qemuOverride
}

type qemuOverride struct {
	Match string
	Repl  string
}

type vmHotplug struct {
	Disk    string
	Version string
}

type KvmVM struct {
	*BaseVM   // embed
	KVMConfig // embed

	// Internal variables
	hotplug map[int]vmHotplug

	pid int
	q   qmp.Conn // qmp connection for this vm

	vncShim net.Listener // shim for VNC connections
	VNCPort int
}

// Ensure that KvmVM implements the VM interface
var _ VM = (*KvmVM)(nil)

var KVMNetworkDrivers struct {
	drivers []string
	sync.Once
}

// Copy makes a deep copy and returns reference to the new struct.
func (old KVMConfig) Copy() KVMConfig {
	// Copy all fields
	res := old

	// Make deep copy of slices
	res.DiskPaths = make([]string, len(old.DiskPaths))
	copy(res.DiskPaths, old.DiskPaths)
	res.QemuAppend = make([]string, len(old.QemuAppend))
	copy(res.QemuAppend, old.QemuAppend)

	return res
}

func NewKVM(name, namespace string, config VMConfig) (*KvmVM, error) {
	vm := new(KvmVM)

	vm.BaseVM = NewBaseVM(name, namespace, config)
	vm.Type = KVM

	vm.KVMConfig = config.KVMConfig.Copy() // deep-copy configured fields

	vm.hotplug = make(map[int]vmHotplug)

	return vm, nil
}

func (vm *KvmVM) Copy() VM {
	vm.lock.Lock()
	defer vm.lock.Unlock()

	vm2 := new(KvmVM)

	// Make shallow copies of all fields
	*vm2 = *vm

	// Make deep copies
	vm2.BaseVM = vm.BaseVM.copy()
	vm2.KVMConfig = vm.KVMConfig.Copy()

	return vm2
}

// Launch a new KVM VM.
func (vm *KvmVM) Launch() error {
	defer vm.lock.Unlock()

	return vm.launch()
}

// Flush cleans up all resources allocated to the VM which includes all the
// network taps.
func (vm *KvmVM) Flush() error {
	vm.lock.Lock()
	defer vm.lock.Unlock()

	for _, net := range vm.Networks {
		// Handle already disconnected taps differently since they aren't
		// assigned to any bridges.
		if net.VLAN == DisconnectedVLAN {
			if err := bridge.DestroyTap(net.Tap); err != nil {
				log.Error("leaked tap %v: %v", net.Tap, err)
			}

			continue
		}

		br, err := getBridge(net.Bridge)
		if err != nil {
			return err
		}

		if err := br.DestroyTap(net.Tap); err != nil {
			log.Error("leaked tap %v: %v", net.Tap, err)
		}
	}

	return vm.BaseVM.Flush()
}

func (vm *KvmVM) Config() *BaseConfig {
	return &vm.BaseConfig
}

func (vm *KvmVM) Start() (err error) {
	vm.lock.Lock()
	defer vm.lock.Unlock()

	if vm.State&VM_RUNNING != 0 {
		return nil
	}

	if vm.State == VM_QUIT || vm.State == VM_ERROR {
		log.Info("relaunching VM: %v", vm.ID)

		// Create a new channel since we closed the other one to indicate that
		// the VM should quit.
		vm.kill = make(chan bool)

		// Launch handles setting the VM to error state
		if err := vm.launch(); err != nil {
			return err
		}
	}

	log.Info("starting VM: %v", vm.ID)
	if err := vm.q.Start(); err != nil {
		log.Errorln(err)
		vm.setError(err)
		return err
	}

	vm.setState(VM_RUNNING)

	return nil
}

func (vm *KvmVM) Stop() error {
	vm.lock.Lock()
	defer vm.lock.Unlock()

	if vm.Name == "vince" {
		return errors.New("vince is unstoppable")
	}

	if vm.State != VM_RUNNING {
		return vmNotRunning(strconv.Itoa(vm.ID))
	}

	log.Info("stopping VM: %v", vm.ID)
	if err := vm.q.Stop(); err != nil {
		log.Errorln(err)
		vm.setError(err)
		return err
	}

	vm.setState(VM_PAUSED)

	return nil
}

func (vm *KvmVM) String() string {
	return fmt.Sprintf("%s:%d:kvm", hostname, vm.ID)
}

func (vm *KvmVM) Info(field string) (string, error) {
	// If the field is handled by BaseVM, return it
	if v, err := vm.BaseVM.Info(field); err == nil {
		return v, nil
	}

	vm.lock.Lock()
	defer vm.lock.Unlock()

	switch field {
	case "vnc_port":
		return strconv.Itoa(vm.VNCPort), nil
	}

	return vm.KVMConfig.Info(field)
}

func (vm *KvmVM) Conflicts(vm2 VM) error {
	switch vm2 := vm2.(type) {
	case *KvmVM:
		return vm.ConflictsKVM(vm2)
	case *ContainerVM:
		return vm.BaseVM.conflicts(vm2.BaseVM)
	}

	return errors.New("unknown VM type")
}

// ConflictsKVM tests whether vm and vm2 share a disk and returns an
// error if one of them is not running in snapshot mode. Also checks
// whether the BaseVMs conflict.
func (vm *KvmVM) ConflictsKVM(vm2 *KvmVM) error {
	vm.lock.Lock()
	defer vm.lock.Unlock()

	for _, d := range vm.DiskPaths {
		for _, d2 := range vm2.DiskPaths {
			if d == d2 && (!vm.Snapshot || !vm2.Snapshot) {
				return fmt.Errorf("disk conflict with vm %v: %v", vm.Name, d)
			}
		}
	}

	return vm.BaseVM.conflicts(vm2.BaseVM)
}

func (vm *KVMConfig) String() string {
	// create output
	var o bytes.Buffer
	w := new(tabwriter.Writer)
	w.Init(&o, 5, 0, 1, ' ', 0)
	fmt.Fprintln(&o, "Current KVM configuration:")
	fmt.Fprintf(w, "Migrate Path:\t%v\n", vm.MigratePath)
	fmt.Fprintf(w, "Disk Paths:\t%v\n", vm.DiskPaths)
	fmt.Fprintf(w, "CDROM Path:\t%v\n", vm.CdromPath)
	fmt.Fprintf(w, "Kernel Path:\t%v\n", vm.KernelPath)
	fmt.Fprintf(w, "Initrd Path:\t%v\n", vm.InitrdPath)
	fmt.Fprintf(w, "Kernel Append:\t%v\n", vm.Append)
	fmt.Fprintf(w, "QEMU Path:\t%v\n", vm.QemuPath)
	fmt.Fprintf(w, "QEMU Append:\t%v\n", vm.QemuAppend)
	fmt.Fprintf(w, "SerialPorts:\t%v\n", vm.SerialPorts)
	fmt.Fprintf(w, "Virtio-SerialPorts:\t%v\n", vm.VirtioPorts)
	w.Flush()
	fmt.Fprintln(&o)
	return o.String()
}

func (vm *KvmVM) QMPRaw(input string) (string, error) {
	return vm.q.Raw(input)
}

func (vm *KvmVM) Migrate(filename string) error {
	path := filepath.Join(*f_iomBase, filename)
	return vm.q.MigrateDisk(path)
}

func (vm *KvmVM) QueryMigrate() (string, float64, error) {
	var status string
	var completed float64

	r, err := vm.q.QueryMigrate()
	if err != nil {
		return "", 0.0, err
	}

	// find the status
	if s, ok := r["status"]; ok {
		status = s.(string)
	} else {
		return status, completed, fmt.Errorf("could not decode status: %v", r)
	}

	var ram map[string]interface{}
	switch status {
	case "completed":
		completed = 100.0
		return status, completed, nil
	case "failed":
		return status, completed, nil
	case "active":
		if e, ok := r["ram"]; !ok {
			return status, completed, fmt.Errorf("could not decode ram segment: %v", e)
		} else {
			switch e.(type) {
			case map[string]interface{}:
				ram = e.(map[string]interface{})
			default:
				return status, completed, fmt.Errorf("invalid ram type: %v", e)
			}
		}
	}

	total := ram["total"].(float64)
	transferred := ram["transferred"].(float64)

	if total == 0.0 {
		return status, completed, fmt.Errorf("zero total ram!")
	}

	completed = transferred / total

	return status, completed, nil
}

func (vm *KvmVM) Screenshot(size int) ([]byte, error) {
	if vm.GetState()&VM_RUNNING == 0 {
		return nil, vmNotRunning(strconv.Itoa(vm.ID))
	}

	suffix := rand.New(rand.NewSource(time.Now().UnixNano())).Int31()
	tmp := filepath.Join(os.TempDir(), fmt.Sprintf("minimega_screenshot_%v", suffix))

	// We have to write this out to a file, because QMP
	err := vm.q.Screendump(tmp)
	if err != nil {
		return nil, err
	}

	ppmFile, err := ioutil.ReadFile(tmp)
	if err != nil {
		return nil, err
	}

	pngResult, err := ppmToPng(ppmFile, size)
	if err != nil {
		return nil, err
	}

	err = os.Remove(tmp)
	if err != nil {
		return nil, err
	}

	return pngResult, nil
}

func (vm *KvmVM) connectQMP() (err error) {
	delay := QMP_CONNECT_DELAY * time.Millisecond

	for count := 0; count < QMP_CONNECT_RETRY; count++ {
		vm.q, err = qmp.Dial(vm.path("qmp"))
		if err == nil {
			log.Debug("qmp dial to %v successful", vm.ID)
			return
		}

		log.Info("qmp dial to %v : %v, redialing in %v", vm.ID, err, delay)
		time.Sleep(delay)
	}

	// Never connected successfully
	return fmt.Errorf("vm %v failed to connect to qmp: %v", vm.ID, err)
}

func (vm *KvmVM) connectVNC() error {
	l, err := net.Listen("tcp", "")
	if err != nil {
		return err
	}

	// Keep track of shim so that we can close it later
	vm.vncShim = l
	vm.VNCPort = l.Addr().(*net.TCPAddr).Port
	ns := fmt.Sprintf("%v:%v", vm.Namespace, vm.Name)

	go func() {
		defer l.Close()

		for {
			// Sit waiting for new connections
			remote, err := l.Accept()
			if err != nil && strings.Contains(err.Error(), "use of closed network connection") {
				return
			} else if err != nil {
				log.Errorln(err)
				return
			}

			log.Info("vnc shim connect: %v -> %v", remote.RemoteAddr(), ns)

			go func() {
				defer remote.Close()

				// Dial domain socket
				local, err := net.Dial("unix", vm.path("vnc"))
				if err != nil {
					log.Error("unable to dial vm vnc: %v", err)
					return
				}
				defer local.Close()

				// copy local -> remote
				go io.Copy(remote, local)

				// Reads will implicitly copy from remote -> local
				tee := io.TeeReader(remote, local)
				for {
					msg, err := vnc.ReadClientMessage(tee)
					if err == nil {
						vncRoute(ns, msg)
						continue
					}

					// shim is no longer connected
					if err == io.EOF || strings.Contains(err.Error(), "broken pipe") {
						log.Info("vnc shim quit: %v", ns)
						break
					}

					// ignore these
					if strings.Contains(err.Error(), "unknown client-to-server message") {
						continue
					}

					// unknown error
					log.Warnln(err)
				}
			}()
		}
	}()

	return nil
}

// launch is the low-level launch function for KVM VMs. The caller should hold
// the VM's lock.
func (vm *KvmVM) launch() error {
	log.Info("launching vm: %v", vm.ID)

	// If this is the first time launching the VM, do the final configuration
	// check and create a directory for it.
	if vm.State == VM_BUILDING {
		if err := os.MkdirAll(vm.instancePath, os.FileMode(0700)); err != nil {
			teardownf("unable to create VM dir: %v", err)
		}
	}

	// write the config for this vm
	config := vm.BaseConfig.String() + vm.KVMConfig.String()
	mustWrite(vm.path("config"), config)
	mustWrite(vm.path("name"), vm.Name)

	// create and add taps if we are associated with any networks
	for i := range vm.Networks {
		nic := &vm.Networks[i]
		if nic.Tap != "" {
			// tap has already been created, don't need to do again
			continue
		}

		br, err := getBridge(nic.Bridge)
		if err != nil {
			log.Error("get bridge: %v", err)
			vm.setError(err)
			return err
		}

		tap, err := br.CreateTap(nic.MAC, nic.VLAN)
		if err != nil {
			log.Error("create tap: %v", err)
			vm.setError(err)
			return err
		}

		nic.Tap = tap
	}

	if len(vm.Networks) > 0 {
		if err := vm.writeTaps(); err != nil {
			log.Errorln(err)
			vm.setError(err)
			return err
		}
	}

	var sOut bytes.Buffer
	var sErr bytes.Buffer

	vmConfig := VMConfig{BaseConfig: vm.BaseConfig, KVMConfig: vm.KVMConfig}
	args := vmConfig.qemuArgs(vm.ID, vm.instancePath)
	args = vmConfig.applyQemuOverrides(args)
	log.Debug("final qemu args: %#v", args)

	path := vm.KVMConfig.QemuPath
	if path == "" {
		p, err := process("kvm")
		if err != nil {
			return err
		}
		path = p
	}

	cmd := &exec.Cmd{
		Path:   path,
		Args:   append([]string{path}, args...),
		Stdout: &sOut,
		Stderr: &sErr,
	}

	if err := cmd.Start(); err != nil {
		err = fmt.Errorf("start qemu: %v %v", err, sErr.String())
		log.Errorln(err)
		vm.setError(err)
		return err
	}

	vm.pid = cmd.Process.Pid
	log.Debug("vm %v has pid %v", vm.ID, vm.pid)

	vm.CheckAffinity()

	// Channel to signal when the process has exited
	var waitChan = make(chan bool)

	// Create goroutine to wait for process to exit
	go func() {
		defer close(waitChan)
		err := cmd.Wait()

		vm.lock.Lock()
		defer vm.lock.Unlock()

		// Check if the process quit for some reason other than being killed
		if err != nil && err.Error() != "signal: killed" {
			log.Error("kill qemu: %v %v", err, sErr.String())
			vm.setError(err)
		} else if vm.State != VM_ERROR {
			// Set to QUIT unless we've already been put into the error state
			vm.setState(VM_QUIT)
		}

		// Kill the VNC shim, if it exists
		if vm.vncShim != nil {
			vm.vncShim.Close()
		}
	}()

	if err := vm.connectQMP(); err != nil {
		// Failed to connect to qmp so clean up the process
		cmd.Process.Kill()

		log.Errorln(err)
		vm.setError(err)
		return err
	}

	go qmpLogger(vm.ID, vm.q)

	if err := vm.connectVNC(); err != nil {
		// Failed to connect to vnc so clean up the process
		cmd.Process.Kill()

		log.Errorln(err)
		vm.setError(err)
		return err
	}

	// connect cc
	ccPath := vm.path("cc")
	if err := ccNode.DialSerial(ccPath); err != nil {
		log.Warn("unable to connect to cc for vm %v: %v", vm.ID, err)
	}

	// Create goroutine to wait to kill the VM
	go func() {
		select {
		case <-waitChan:
			log.Info("VM %v exited", vm.ID)
		case <-vm.kill:
			log.Info("Killing VM %v", vm.ID)
			cmd.Process.Kill()
			<-waitChan
			killAck <- vm.ID
		}
	}()

	return nil
}

func (vm *KvmVM) Hotplug(f, version string) error {
	var bus string
	switch version {
	case "", "1.1":
		version = "1.1"
		bus = "usb-bus.0"
	case "2.0":
		bus = "ehci.0"
	default:
		return fmt.Errorf("invalid version: `%v`", version)
	}

	vm.lock.Lock()
	defer vm.lock.Unlock()

	// generate an id by adding 1 to the highest in the list for the
	// hotplug devices, 0 if it's empty
	id := 0
	for k := range vm.hotplug {
		if k >= id {
			id = k + 1
		}
	}

	hid := fmt.Sprintf("hotplug%v", id)
	log.Debugln("hotplug generated id:", hid)

	r, err := vm.q.DriveAdd(hid, f)
	if err != nil {
		return err
	}
	log.Debugln("hotplug drive_add response:", r)

	r, err = vm.q.USBDeviceAdd(hid, bus)
	if err != nil {
		return err
	}

	log.Debugln("hotplug usb device add response:", r)
	vm.hotplug[id] = vmHotplug{f, version}

	return nil
}

func (vm *KvmVM) HotplugRemoveAll() error {
	vm.lock.Lock()
	defer vm.lock.Unlock()

	if len(vm.hotplug) == 0 {
		return errors.New("no hotplug devices to remove")
	}

	for k := range vm.hotplug {
		if err := vm.hotplugRemove(k); err != nil {
			return err
		}
	}

	return nil
}

func (vm *KvmVM) HotplugRemove(id int) error {
	vm.lock.Lock()
	defer vm.lock.Unlock()

	return vm.hotplugRemove(id)
}

func (vm *KvmVM) hotplugRemove(id int) error {
	hid := fmt.Sprintf("hotplug%v", id)
	log.Debugln("hotplug id:", hid)
	if _, ok := vm.hotplug[id]; !ok {
		return errors.New("no such hotplug device")
	}

	resp, err := vm.q.USBDeviceDel(hid)
	if err != nil {
		return err
	}

	log.Debugln("hotplug usb device del response:", resp)
	resp, err = vm.q.DriveDel(hid)
	if err != nil {
		return err
	}

	log.Debugln("hotplug usb drive del response:", resp)
	delete(vm.hotplug, id)
	return nil
}

// HotplugInfo returns a deep copy of the VM's hotplug info
func (vm *KvmVM) HotplugInfo() map[int]vmHotplug {
	vm.lock.Lock()
	defer vm.lock.Unlock()

	res := map[int]vmHotplug{}

	for k, v := range vm.hotplug {
		res[k] = vmHotplug{v.Disk, v.Version}
	}

	return res
}

func (vm *KvmVM) ChangeCD(f string) error {
	vm.lock.Lock()
	defer vm.lock.Unlock()

	if vm.CdromPath != "" {
		if err := vm.ejectCD(); err != nil {
			return err
		}
	}

	err := vm.q.BlockdevChange("ide0-cd1", f)
	if err == nil {
		vm.CdromPath = f
	}

	return err
}

func (vm *KvmVM) EjectCD() error {
	vm.lock.Lock()
	defer vm.lock.Unlock()

	if vm.CdromPath == "" {
		return errors.New("no cdrom inserted")
	}

	return vm.ejectCD()
}

func (vm *KvmVM) ejectCD() error {
	err := vm.q.BlockdevEject("ide0-cd1")
	if err == nil {
		vm.CdromPath = ""
	}

	return err
}

func (vm *KvmVM) ProcStats() (map[int]*ProcStats, error) {
	p, err := GetProcStats(vm.pid)
	if err != nil {
		return nil, err
	}

	return map[int]*ProcStats{vm.pid: p}, nil
}

// qemuArgs build the horribly long qemu argument string
//
// Note: it would be cleaner if this was a method for KvmVM rather than
// VMConfig but we want to be able to show the qemu arg string before and after
// overrides in the `vm config qemu-override` API. We cannot use KVMConfig as
// the receiver either because we need to look at fields from the BaseConfig to
// build the qemu args.
func (vm VMConfig) qemuArgs(id int, vmPath string) []string {
	var args []string

	args = append(args, "-enable-kvm")

	args = append(args, "-name")
	args = append(args, strconv.Itoa(id))

	args = append(args, "-m")
	args = append(args, strconv.FormatUint(vm.Memory, 10))

	args = append(args, "-nographic")

	args = append(args, "-balloon")
	args = append(args, "none")

	args = append(args, "-vnc")
	args = append(args, "unix:"+filepath.Join(vmPath, "vnc"))

	args = append(args, "-smp")
	args = append(args, strconv.FormatUint(vm.VCPUs, 10))

	args = append(args, "-qmp")
	args = append(args, "unix:"+filepath.Join(vmPath, "qmp")+",server")

	args = append(args, "-vga")
	args = append(args, "std")

	args = append(args, "-rtc")
	args = append(args, "clock=vm,base=utc")

	args = append(args, "-device")
	args = append(args, "virtio-serial")

	// for USB 1.0, creates bus named usb-bus.0
	args = append(args, "-usb")
	// for USB 2.0, creates bus named ehci.0
	args = append(args, "-device", "usb-ehci,id=ehci")
	// this allows absolute pointers in vnc, and works great on android vms
	args = append(args, "-device", "usb-tablet,bus=usb-bus.0")

	// this is non-virtio serial ports
	// for virtio-serial, look below near the net code
	for i := uint64(0); i < vm.SerialPorts; i++ {
		args = append(args, "-chardev")
		args = append(args, fmt.Sprintf("socket,id=charserial%v,path=%v%v,server,nowait", i, filepath.Join(vmPath, "serial"), i))

		args = append(args, "-device")
		args = append(args, fmt.Sprintf("isa-serial,chardev=charserial%v,id=serial%v", i, i))
	}

	args = append(args, "-pidfile")
	args = append(args, filepath.Join(vmPath, "qemu.pid"))

	args = append(args, "-k")
	args = append(args, "en-us")

	if vm.CPU != "" {
		args = append(args, "-cpu")
		args = append(args, vm.CPU)
	}

	args = append(args, "-net")
	args = append(args, "none")

	args = append(args, "-S")

	if vm.MigratePath != "" {
		args = append(args, "-incoming")
		args = append(args, fmt.Sprintf("exec:cat %v", vm.MigratePath))
	}

	if len(vm.DiskPaths) != 0 {
		for _, diskPath := range vm.DiskPaths {
			args = append(args, "-drive")
			args = append(args, "file="+diskPath+",media=disk")
		}
	}

	if vm.Snapshot {
		args = append(args, "-snapshot")
	}

	if vm.KernelPath != "" {
		args = append(args, "-kernel")
		args = append(args, vm.KernelPath)
	}
	if vm.InitrdPath != "" {
		args = append(args, "-initrd")
		args = append(args, vm.InitrdPath)
	}
	if len(vm.Append) > 0 {
		args = append(args, "-append")
		args = append(args, unescapeString(vm.Append))
	}

	if vm.CdromPath != "" {
		args = append(args, "-drive")
		args = append(args, "file="+vm.CdromPath+",media=cdrom")
		args = append(args, "-boot")
		args = append(args, "once=d")
	} else {
		// add an empty cdrom
		args = append(args, "-drive")
		args = append(args, "media=cdrom")
	}

	// net
	var bus, addr int
	addBus := func() {
		addr = 1 // start at 1 because 0 is reserved
		bus++
		args = append(args, fmt.Sprintf("-device"))
		args = append(args, fmt.Sprintf("pci-bridge,id=pci.%v,chassis_nr=%v", bus, bus))
	}

	addBus()
	for _, net := range vm.Networks {
		args = append(args, "-netdev")
		args = append(args, fmt.Sprintf("tap,id=%v,script=no,ifname=%v", net.Tap, net.Tap))
		args = append(args, "-device")
		args = append(args, fmt.Sprintf("driver=%v,netdev=%v,mac=%v,bus=pci.%v,addr=0x%x", net.Driver, net.Tap, net.MAC, bus, addr))
		addr++
		if addr == DEV_PER_BUS {
			addBus()
		}
	}

	// virtio-serial
	// we always get a cc virtio port
	args = append(args, "-device")
	args = append(args, fmt.Sprintf("virtio-serial-pci,id=virtio-serial0,bus=pci.%v,addr=0x%x", bus, addr))
	args = append(args, "-chardev")
	args = append(args, fmt.Sprintf("socket,id=charvserialCC,path=%v,server,nowait", filepath.Join(vmPath, "cc")))
	args = append(args, "-device")
	args = append(args, fmt.Sprintf("virtserialport,nr=1,bus=virtio-serial0.0,chardev=charvserialCC,id=charvserialCC,name=cc"))
	addr++
	if addr == DEV_PER_BUS { // check to see if we've run out of addr slots on this bus
		addBus()
	}

	virtio_slot := 0 // start at 0 since we immediately increment and we already have a cc port
	for i := uint64(0); i < vm.VirtioPorts; i++ {
		// qemu port number
		nr := i%DEV_PER_VIRTIO + 1

		// If port is 1, we're out of slots on the current virtio-serial-pci
		// device or we're on the first iteration => make a new device
		if nr == 1 {
			virtio_slot++
			args = append(args, "-device")
			args = append(args, fmt.Sprintf("virtio-serial-pci,id=virtio-serial%v,bus=pci.%v,addr=0x%x", virtio_slot, bus, addr))

			addr++
			if addr == DEV_PER_BUS { // check to see if we've run out of addr slots on this bus
				addBus()
			}
		}

		args = append(args, "-chardev")
		args = append(args, fmt.Sprintf("socket,id=charvserial%v,path=%v%v,server,nowait", i, filepath.Join(vmPath, "virtio-serial"), i))

		args = append(args, "-device")
		args = append(args, fmt.Sprintf("virtserialport,nr=%v,bus=virtio-serial%v.0,chardev=charvserial%v,id=charvserial%v,name=virtio-serial%v", nr, virtio_slot, i, i, i))
	}

	// hook for hugepage support
	if hugepagesMountPath != "" {
		args = append(args, "-mem-info")
		args = append(args, hugepagesMountPath)
	}

	if len(vm.QemuAppend) > 0 {
		args = append(args, vm.QemuAppend...)
	}

	args = append(args, "-uuid")
	args = append(args, vm.UUID)

	log.Debug("args for vm %v are: %#v", id, args)
	return args
}

func (vm VMConfig) qemuOverrideString() string {
	// create output
	var o bytes.Buffer
	w := new(tabwriter.Writer)
	w.Init(&o, 5, 0, 1, ' ', 0)
	fmt.Fprintln(&o, "id\tmatch\treplacement")
	for i, v := range vm.QemuOverride {
		fmt.Fprintf(&o, "%v\t\"%v\"\t\"%v\"\n", i, v.Match, v.Repl)
	}
	w.Flush()

	args := vm.qemuArgs(0, "") // ID and path don't matter -- just testing
	preArgs := unescapeString(args)
	postArgs := unescapeString(vm.applyQemuOverrides(args))

	r := o.String()
	r += fmt.Sprintf("\nBefore overrides:\n%v\n", preArgs)
	r += fmt.Sprintf("\nAfter overrides:\n%v\n", postArgs)

	return r
}

func (vm VMConfig) applyQemuOverrides(args []string) []string {
	ret := unescapeString(args)
	for _, v := range vm.QemuOverride {
		ret = strings.Replace(ret, v.Match, v.Repl, -1)
	}
	return fieldsQuoteEscape("\"", ret)
}

// log any asynchronous messages, such as vnc connects, to log.Info
func qmpLogger(id int, q qmp.Conn) {
	for v := q.Message(); v != nil; v = q.Message() {
		log.Info("VM %v received asynchronous message: %v", id, v)
	}
}

func isNetworkDriver(driver string) bool {
	KVMNetworkDrivers.Do(func() {
		drivers := []string{}

		out, err := processWrapper("kvm", "-device", "help")
		if err != nil {
			log.Error("unable to determine kvm network drivers -- %v", err)
			return
		}

		var foundHeader bool

		scanner := bufio.NewScanner(strings.NewReader(out))
		for scanner.Scan() {
			line := scanner.Text()
			if !foundHeader && strings.Contains(line, "Network devices:") {
				foundHeader = true
			} else if foundHeader && line == "" {
				break
			} else if foundHeader {
				parts := strings.Split(line, " ")
				driver := strings.Trim(parts[1], `",`)
				drivers = append(drivers, driver)
			}
		}

		if err := scanner.Err(); err != nil {
			log.Error("unable to determine kvm network drivers -- %v", err)
			return
		}

		log.Debug("detected network drivers: %v", drivers)
		KVMNetworkDrivers.drivers = drivers
	})

	for _, d := range KVMNetworkDrivers.drivers {
		if d == driver {
			return true
		}
	}

	return false
}
