// Copyright (2013) Sandia Corporation.
// Under the terms of Contract DE-AC04-94AL85000 with Sandia Corporation,
// the U.S. Government retains certain rights in this software.

// igor is a simple command line tool for managing reservations of nodes in a
// cluster. It also will configure the pxeboot environment for booting kernels
// and initrds for cluster nodes.
package main

import (
	"encoding/gob"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	log "minilog"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// Constants

// The length of a single time-slice in the schedule.
// Must be less than 60! 1, 5, 10, or 15 are good choices
// Shorter length means less waiting for reservations to start, but bigger schedule files.
const MINUTES_PER_SLICE = 1

// Minimum schedule length in minutes, 720 = 12 hours
const MIN_SCHED_LEN = 720

// Global Variables
// This flag can be set regardless of which subcommand is executed
var configpath = flag.String("config", "/etc/igor.conf", "Path to configuration file")

// The configuration we read in from the file
var igorConfig Config

// Our most important data structures: the reservation list, and the schedule
var Reservations map[uint64]Reservation // map ID to reservations
var Schedule []TimeSlice                // The schedule

// The files from which we read the reservations & schedule
var resdb *os.File
var scheddb *os.File

// Commands lists the available commands and help topics.
// The order here is the order in which they are printed by 'go help'.
var commands = []*Command{
	cmdDel,
	cmdShow,
	cmdSub,
	cmdPower,
	cmdExtend,
	cmdNotify,
}

var exitStatus = 0
var exitMu sync.Mutex

// The configuration of the system
type Config struct {
	// TFTPRoot is where the igor configs are stored.
	// It should be the root of your TFTP server if not using Cobbler
	// If using Cobbler, it should be /var/lib/igor
	TFTPRoot string
	// The prefix for cluster nodes, e.g. 'kn' if nodes are named kn01 etc.
	Prefix string
	// The first node number in the cluster, (usually 1)
	Start int
	// The last node number in the cluster
	End int
	// How wide the numeric part of a node name must be padded.
	// If you have a node named kn001, set Padlen to 3
	// If you have one named kn1, set it to 0.
	Padlen int
	// Width and height of each rack in the cluster. Only used for display purposes
	Rackwidth  int
	Rackheight int
	// printf-formatted string to power on/off a single node
	// e.g. "powerman on %s"
	PowerOnCommand  string
	PowerOffCommand string
	// True if using Cobbler to manage nodes
	UseCobbler bool
	// If using Cobbler, nodes not in a reservation will be set to this profile
	CobblerDefaultProfile string
	// If set to true, nodes will be automatically rebooted when
	// the reservation starts, if possible
	AutoReboot bool
	// VLAN segmentation options
	// VLANMin/VLANMax: specify a range of VLANs to use
	// NodeMap: maps hostnames to switch port names
	// Network: selects which type of switch is in use. Set to "" to disable VLAN segmentation
	// NetworkUser/NetworkPassword: login info for a switch user capable of configuring ports
	// NetworkURL: HTTP URL for sending API commands to the switch
	VLANMin         int               `json:"vlan_min"`
	VLANMax         int               `json:"vlan_max"`
	NodeMap         map[string]string `json:"node_map"`
	Network         string
	NetworkUser     string
	NetworkPassword string
	NetworkURL      string `json:"network_url"`
	// Set this to a DNS server if multiple servers are available and hostname lookups are failing
	DNSServer string
	// A file to receive log info
	LogFile string
	// NodeLimit: max nodes a non-root user can reserve
	// TimeLimit: max time a non-root user can reserve
	NodeLimit int
	TimeLimit int

	// Domain for email address
	Domain string
}

// Represents a slice of time in the Schedule
type TimeSlice struct {
	Start int64    // UNIX time
	End   int64    // UNIX time
	Nodes []uint64 // slice of len(# of nodes), mapping to reservation IDs
}

// Sort the slice of reservations based on the start time
type StartSorter []Reservation

func (s StartSorter) Len() int {
	return len(s)
}

func (s StartSorter) Less(i, j int) bool {
	return s[i].StartTime < s[j].StartTime
}

func (s StartSorter) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}

func setExitStatus(n int) {
	exitMu.Lock()
	if exitStatus < n {
		exitStatus = n
	}
	exitMu.Unlock()
}

// Runs at startup to handle automated tasks that need to happen now.
// Read the reservations, delete any that are too old.
// Copy in netboot files for any reservations that have just started
func housekeeping() {
	now := time.Now().Unix()

	backend := GetBackend()

	for _, r := range Reservations {
		// Check if $TFTPROOT/pxelinux.cfg/igor/ResName exists. This is how we verify if the reservation is installed or not
		if r.EndTime < now {
			// Reservation expired; delete it
			deleteReservation(false, []string{r.ResName})
		} else if _, err := os.Stat(r.Filename()); os.IsNotExist(err) && r.StartTime < now {
			// Reservation should have started but has not yet been installed
			emitReservationLog("INSTALL", r)
			// update network config
			err := networkSet(r.Hosts, r.Vlan)
			if err != nil {
				log.Error("error setting network isolation: %v", err)
			}

			if err := backend.Install(r); err != nil {
				log.Fatal("unable to install: %v", err)
			}

			if igorConfig.AutoReboot {
				if err := backend.Power(r.Hosts, false); err != nil {
					log.Fatal("unable to power off: %v", err)
				}

				if err := backend.Power(r.Hosts, true); err != nil {
					log.Fatal("unable to power on: %v", err)
				}
			}
		}
	}

	// Clean up the schedule and write it out
	expireSchedule()
	putSchedule()
}

func init() {
	Reservations = make(map[uint64]Reservation)
}

func main() {
	var err error

	log.Init()

	flag.Usage = usage
	flag.Parse()

	args := flag.Args()
	if len(args) < 1 {
		usage()
	}

	if args[0] == "help" {
		help(args[1:])
		return
	}

	if args[0] == "version" {
		printVersion()
		return
	}

	rand.Seed(time.Now().UnixNano())

	igorConfig = readConfig(*configpath)

	// Add another logger for the logfile, if set
	if igorConfig.LogFile != "" {
		logfile, err := os.OpenFile(igorConfig.LogFile, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0660)
		if err != nil {
			log.Fatal("Couldn't create logfile %v: %v", igorConfig.LogFile, err)
		}
		log.AddLogger("file", logfile, log.INFO, false)
	}

	// Read in the reservations
	// We open the file here so resdb.Close() doesn't happen until program exit
	path := filepath.Join(igorConfig.TFTPRoot, "/igor/reservations.json")
	resdb, err = os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0664)
	if err != nil {
		log.Fatal("failed to open reservations file: %v", err)
	}
	defer resdb.Close()
	// This should prevent anyone else from modifying the reservation file while
	// we're using it. Bonus: Flock goes away if the program crashes so state is easy
	if err := syscall.Flock(int(resdb.Fd()), syscall.LOCK_EX); err != nil {
		// TODO: should we wait?
		log.Fatal("unable to lock reservations file -- someone else is running igor")
	}
	defer syscall.Flock(int(resdb.Fd()), syscall.LOCK_UN) // this will unlock it later

	getReservations()

	// Read in the schedule
	path = filepath.Join(igorConfig.TFTPRoot, "/igor/schedule.gob")
	scheddb, err = os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0664)
	if err != nil {
		log.Warn("failed to open schedule file: %v", err)
	}
	defer scheddb.Close()
	// We probably don't need to lock this too but I'm playing it safe
	if err := syscall.Flock(int(scheddb.Fd()), syscall.LOCK_EX); err != nil {
		// TODO: should we wait?
		log.Fatal("unable to lock schedule file -- someone else is running igor")
	}
	defer syscall.Flock(int(scheddb.Fd()), syscall.LOCK_UN) // this will unlock it later
	getSchedule()

	// Here, we need to go through and delete any reservations which should be expired,
	// and bring in new ones that are just starting
	housekeeping()

	// Now process the command
	for _, cmd := range commands {
		if cmd.Name() == args[0] && cmd.Run != nil {
			cmd.Flag.Usage = func() { cmd.Usage() }
			if cmd.CustomFlags {
				args = args[1:]
			} else {
				cmd.Flag.Parse(args[1:])
				args = cmd.Flag.Args()
			}
			cmd.Run(cmd, args)
			return
		}
	}

	fmt.Fprintf(os.Stderr, "go: unknown subcommand %q\nRun 'go help' for usage.\n", args[0])
	setExitStatus(2)
}

// Read in the reservations from the already-open resdb file
func getReservations() {
	dec := json.NewDecoder(resdb)
	err := dec.Decode(&Reservations)
	// an empty file is OK, but other errors are not
	if err != nil && err != io.EOF {
		log.Fatal("failure parsing reservation file: %v", err)
	}
}

// Read in the schedule from the already-open schedule file
func getSchedule() {
	dec := gob.NewDecoder(scheddb)
	err := dec.Decode(&Schedule)
	// an empty file is OK, but other errors are not
	if err != nil && err != io.EOF {
		log.Fatal("failure parsing schedule file: %v", err)
	}
}

// Write out the reservations
func putReservations() {
	// Truncate the existing reservation file
	resdb.Truncate(0)
	resdb.Seek(0, 0)

	// Write out the new reservations
	if err := json.NewEncoder(resdb).Encode(Reservations); err != nil {
		log.Fatal("unable to encode reservations: %v", err)
	}

	resdb.Sync()
}

// Write out the schedule
func putSchedule() {
	// Truncate the existing schedule file
	scheddb.Truncate(0)
	scheddb.Seek(0, 0)

	// Write out the new schedule
	if err := gob.NewEncoder(scheddb).Encode(Schedule); err != nil {
		log.Fatal("unable to encode schedule: %v", err)
	}

	scheddb.Sync()
}

// Read in the configuration from the specified path.
func readConfig(path string) (c Config) {
	b, err := ioutil.ReadFile(path)
	if err != nil {
		log.Fatal("Couldn't read config file: %v", err)
	}

	err = json.Unmarshal(b, &c)
	if err != nil {
		log.Fatal("Couldn't parse json: %v", err)
	}
	return
}
