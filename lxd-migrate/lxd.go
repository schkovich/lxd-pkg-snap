package main

import (
	"bufio"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/lxc/lxd/client"
	"github.com/lxc/lxd/shared"
	"github.com/lxc/lxd/shared/api"
)

type lxdDaemon struct {
	s    lxd.ContainerServer
	path string

	info         *api.Server
	containers   []api.Container
	images       []api.Image
	networks     []api.Network
	storagePools []api.StoragePool
}

func lxdConnect(path string) (*lxdDaemon, error) {
	// Connect to the LXD daemon
	s, err := lxd.ConnectLXDUnix(fmt.Sprintf("%s/unix.socket", path), nil)
	if err != nil {
		return nil, err
	}

	// Setup our internal struct
	d := &lxdDaemon{s: s, path: path}

	// Get a bunch of data from the daemon
	err = d.update()
	if err != nil {
		return nil, err
	}

	return d, nil
}

func (d *lxdDaemon) update() error {
	// Daemon
	info, _, err := d.s.GetServer()
	if err != nil {
		return err
	}

	d.info = info

	// Containers
	containers, err := d.s.GetContainers()
	if err != nil {
		return err
	}

	d.containers = containers

	// Images
	images, err := d.s.GetImages()
	if err != nil {
		return err
	}

	d.images = images

	// Networks
	if d.s.HasExtension("network") {
		networks, err := d.s.GetNetworks()
		if err != nil {
			return err
		}

		// We only care about the managed ones
		d.networks = []api.Network{}
		for _, network := range networks {
			if network.Managed {
				d.networks = append(d.networks, network)
				break
			}
		}
	}

	// Storage pools
	if d.s.HasExtension("storage") {
		pools, err := d.s.GetStoragePools()
		if err != nil {
			return err
		}

		d.storagePools = pools
	}

	return nil
}

func (d *lxdDaemon) checkEmpty() error {
	// Containers
	if len(d.containers) > 0 {
		return fmt.Errorf("Target LXD already has containers, aborting.")
	}

	// Images
	if len(d.images) > 0 {
		return fmt.Errorf("Target LXD already has images, aborting.")
	}

	// Networks
	if d.networks != nil {
		if len(d.networks) > 0 {
			return fmt.Errorf("Target LXD already has networks, aborting.")
		}
	}

	// Storage pools
	if d.storagePools != nil {
		if len(d.storagePools) > 0 {
			return fmt.Errorf("Target LXD already has storage pools, aborting.")
		}
	}

	return nil
}

func (d *lxdDaemon) showReport() error {
	// Print a basic report to the console
	fmt.Printf("LXD version: %s\n", d.info.Environment.ServerVersion)
	fmt.Printf("LXD PID: %d\n", d.info.Environment.ServerPid)
	fmt.Printf("Resources:\n")
	fmt.Printf("  Containers: %d\n", len(d.containers))
	fmt.Printf("  Images: %d\n", len(d.images))
	if d.networks != nil {
		fmt.Printf("  Networks: %d\n", len(d.networks))
	}
	if d.storagePools != nil {
		fmt.Printf("  Storage pools: %d\n", len(d.storagePools))
	}

	return nil
}

func (d *lxdDaemon) shutdown() error {
	// Send the shutdown request
	_, _, err := d.s.RawQuery("PUT", "/internal/shutdown", nil, "")
	if err != nil {
		return err
	}

	// Wait for the daemon to exit
	chMonitor := make(chan bool, 1)
	go func() {
		monitor, err := d.s.GetEvents()
		if err != nil {
			close(chMonitor)
			return
		}

		monitor.Wait()
		close(chMonitor)
	}()

	// Wait for the daemon to exit or timeout to be reached
	select {
	case <-chMonitor:
		break
	case <-time.After(time.Second * time.Duration(300)):
		return fmt.Errorf("LXD still running after 5 minutes")
	}

	return nil
}

func (d *lxdDaemon) wait() error {
	finger := make(chan error, 1)
	go func() {
		for {
			c, err := lxd.ConnectLXDUnix(filepath.Join(d.path, "unix.socket"), nil)
			if err != nil {
				time.Sleep(500 * time.Millisecond)
				continue
			}

			_, _, err = c.RawQuery("GET", "/internal/ready", nil, "")
			if err != nil {
				time.Sleep(500 * time.Millisecond)
				continue
			}

			finger <- nil
			return
		}
	}()

	select {
	case <-finger:
		break
	case <-time.After(time.Second * time.Duration(300)):
		return fmt.Errorf("LXD still not running after 5 minutes.")
	}

	return nil
}

func (d *lxdDaemon) reload() error {
	// Reload or restart the relevant systemd units
	if strings.HasPrefix(d.path, "/var/snap") {
		return systemdCtl("reload", "snap.lxd.daemon.service")
	}

	if osInit() == "upstart" {
		return upstartCtl("restart", "lxd")
	}

	return systemdCtl("restart", "lxd.service", "lxd.socket")
}

func (d *lxdDaemon) start() error {
	// Start the relevant systemd units
	if strings.HasPrefix(d.path, "/var/snap") {
		return systemdCtl("start", "snap.lxd.daemon.service")
	}

	if osInit() == "upstart" {
		return upstartCtl("start", "lxd")
	}

	return systemdCtl("start", "lxd.service", "lxd.socket")
}

func (d *lxdDaemon) stop() error {
	// Stop the relevant systemd units
	if strings.HasPrefix(d.path, "/var/snap") {
		return systemdCtl("stop", "snap.lxd.daemon.service")
	}

	if osInit() == "upstart" {
		return upstartCtl("stop", "lxd")
	}

	return systemdCtl("stop", "lxd.service", "lxd.socket")
}

func (d *lxdDaemon) uninstall() error {
	// Remove the LXD package
	if strings.HasPrefix(d.path, "/var/snap") {
		_, err := shared.RunCommand("snap", "remove", "lxd")
		return err
	}

	_, err := shared.RunCommand("apt-get", "remove", "--purge", "--yes", "lxd", "lxd-client")
	return err
}

func (d *lxdDaemon) wipe() error {
	// Check if the path is already gone
	if !shared.PathExists(d.path) {
		return nil
	}

	return os.RemoveAll(d.path)
}

func (d *lxdDaemon) moveFiles(dst string) error {
	src := d.path

	// If the daemon is on its own mounpoint, transfer its content one by one
	if shared.IsMountPoint(src) {
		err := os.MkdirAll(dst, 0755)
		if err != nil {
			return err
		}

		entries, err := ioutil.ReadDir(src)
		if err != nil {
			return err
		}

		for _, entry := range entries {
			_, err := shared.RunCommand("mv", filepath.Join(src, entry.Name()), filepath.Join(dst, entry.Name()))
			if err != nil {
				return err
			}
		}
	}

	// Move the daemon path to a new target
	_, err := shared.RunCommand("mv", src, dst)
	if err != nil {
		return err
	}

	// Create the logs directory if missing (needed by LXD)
	if !shared.PathExists(filepath.Join(dst, "logs")) {
		err := os.MkdirAll(filepath.Join(dst, "logs"), 0755)
		if err != nil {
			return err
		}
	}

	return nil
}

func (d *lxdDaemon) cleanMounts() error {
	mounts := []string{}

	// Get all the mounts under the daemon path
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		s := strings.Split(scanner.Text(), " ")
		if strings.HasPrefix(s[4], d.path) {
			mounts = append(mounts, s[4])
		}
	}

	// Reverse the list
	sort.Sort(sort.Reverse(sort.StringSlice(mounts)))

	// Attempt to lazily unmount them all
	for _, mount := range mounts {
		if mount == d.path {
			continue
		}

		err = syscall.Unmount(mount, syscall.MNT_DETACH)
		if err != nil {
			return fmt.Errorf("Unable to unmount: %s: %v", mount, err)
		}
	}

	return nil
}

func (d *lxdDaemon) rewriteStorage(db *dbInstance, dst string) error {
	// Symlink rewrite function
	rewriteSymlink := func(path string) error {
		target, err := os.Readlink(path)
		if err != nil {
			// Not a symlink, skipping
			return nil
		}

		newTarget := convertPath(target, d.path, dst)
		if target != newTarget {
			err = os.Remove(path)
			if err != nil {
				return err
			}

			err = os.Symlink(newTarget, path)
			if err != nil {
				return err
			}
		}

		return nil
	}

	// ZFS rewrite function
	zfsRewrite := func(zpool string) error {
		output, err := shared.RunCommand("zfs", "list", "-H", "-t", "all", "-o", "name,mountpoint", "-r", zpool)
		if err != nil {
			return err
		}

		for _, line := range strings.Split(output, "\n") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}

			name := fields[0]
			mountpoint := fields[1]

			if mountpoint == "none" || mountpoint == "-" {
				continue
			}

			mountpoint = convertPath(mountpoint, d.path, dst)
			_, err := shared.RunCommand("zfs", "set", fmt.Sprintf("mountpoint=%s", mountpoint), name)
			if err != nil {
				return err
			}
		}

		return nil
	}

	// Rewrite the container links
	containers, err := ioutil.ReadDir(filepath.Join(dst, "containers"))
	if err != nil {
		return err
	}

	for _, ctn := range containers {
		err := rewriteSymlink(filepath.Join(dst, "containers", ctn.Name()))
		if err != nil {
			return err
		}
	}

	// Handle older LXD daemons
	if d.storagePools == nil {
		zpool, ok := d.info.Config["storage.zfs_pool_name"]
		if ok {
			err := zfsRewrite(zpool.(string))
			if err != nil {
				return err
			}
		}

		return nil
	}

	for _, pool := range d.storagePools {
		source := pool.Config["source"]
		newSource := convertPath(source, d.path, dst)
		if source != newSource {
			err := db.updateStoragePoolSource(pool.Name, newSource)
			if err != nil {
				return err
			}

			pool.Config["source"] = newSource
		}

		if pool.Driver == "zfs" {
			// For ZFS we must rewrite all the mountpoints
			zpool := pool.Config["zfs.pool_name"]
			err = zfsRewrite(zpool)
			if err != nil {
				return err
			}

			continue
		}

		if pool.Driver == "dir" {
			// For dir we must rewrite any symlink
			err := rewriteSymlink(filepath.Join(dst, "storage-pools", pool.Name))
			if err != nil {
				return err
			}

			continue
		}
	}

	return nil
}
