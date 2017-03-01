// Copyright (2012) Sandia Corporation.
// Under the terms of Contract DE-AC04-94AL85000 with Sandia Corporation,
// the U.S. Government retains certain rights in this software.

package main

import (
	"flag"
	"fmt"
	"golang.org/x/crypto/ssh/terminal"
	"io"
	"io/ioutil"
	log "minilog"
	"os"
	"os/exec"
	"syscall"
)

var (
	f_keys = flag.String("keys", "", "authorized_keys formatted file to install for root")
)

func usage() {
	fmt.Println("usage: passwordify [option]... [source initramfs] [destination initramfs]")
	flag.PrintDefaults()
}

func main() {
	flag.Usage = usage
	flag.Parse()

	log.Init()

	if flag.NArg() != 2 {
		usage()
		os.Exit(1)
	}

	source := flag.Arg(0)
	destination := flag.Arg(1)

	// Make sure the source exists
	if _, err := os.Stat(source); os.IsNotExist(err) {
		log.Fatalln("cannot find source initramfs", source, ":", err)
	}

	// Working directory
	tdir, err := ioutil.TempDir("", "passwordify")
	if err != nil {
		log.Fatalln("Cannot create tempdir:", err)
	}

	// Unpack initrd
	initrdCommand := fmt.Sprintf("cd %v && zcat %v | cpio -idmv", tdir, source)
	err = runscript(initrdCommand)
	if err != nil {
		log.Fatalln(err)
	}

	// Set password
	p := process("chroot")
	cmd := &exec.Cmd{
		Path: p,
		Args: []string{
			p,
			tdir,
			"passwd",
		},
		Env: nil,
		Dir: "",
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Fatalln(err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		log.Fatalln(err)
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		log.Fatalln(err)
	}

	go func() {
		defer stdin.Close()
		fmt.Printf("Enter new root password: ")
		pw1, err := terminal.ReadPassword(int(syscall.Stdin))
		if err != nil {
			log.Fatalln(err)
		}
		fmt.Printf("\nRetype new root password: ")
		pw2, err := terminal.ReadPassword(int(syscall.Stdin))
		if err != nil {
			log.Fatalln(err)
		}
		fmt.Printf("\n")
		if string(pw1) != string(pw2) {
			log.Fatalln("passwords do not match")
		}
		io.WriteString(stdin, string(pw1)+"\n")
		io.WriteString(stdin, string(pw1)+"\n")
	}()

	log.LogAll(stdout, log.INFO, "chroot")
	log.LogAll(stderr, log.INFO, "chroot")

	cmd.Run()

	// If keyfile, copy keyfile
	if *f_keys != "" {
		in, err := os.Open(*f_keys)
		if err != nil {
			log.Fatalln("can't open key file source:", err)
		}
		defer in.Close()

		err = os.Mkdir(tdir+"/root/.ssh", os.ModeDir|0700)
		if err != nil {
			log.Fatalln("can't make root's ssh directory:", err)
		}

		out, err := os.OpenFile(tdir+"/root/.ssh/authorized_keys", os.O_RDWR|os.O_CREATE, 0600)
		if err != nil {
			log.Fatalln("Can't open authorized_keys file:", err)
		}
		defer out.Close()

		if _, err = io.Copy(out, in); err != nil {
			log.Fatalln("Couldn't copy to authorized_keys:", err)
		}
		out.Sync()
	}

	// Repack initrd
	initrdCommand = fmt.Sprintf("cd %v && find . -print0 | cpio --quiet  --null -ov --format=newc | gzip -9 > %v", tdir, destination)
	err = runscript(initrdCommand)
	if err != nil {
		log.Fatalln(err)
	}

	// Cleanup
	err = os.RemoveAll(tdir)
	if err != nil {
		log.Fatalln(err)
	}
}

func runscript(cmdString string) error {
	f, err := ioutil.TempFile("", "passwordify_cmd")
	if err != nil {
		return err
	}

	eName := f.Name()

	f.WriteString(cmdString)
	f.Close()

	log.Debugln("initrd command:", cmdString)

	p := process("bash")
	cmd := exec.Command(p, eName)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	log.LogAll(stdout, log.INFO, "cpio")
	log.LogAll(stderr, log.INFO, "cpio")

	err = cmd.Run()
	if err != nil {
		return err
	}
	os.Remove(eName)

	return nil
}
