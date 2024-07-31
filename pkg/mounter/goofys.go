package mounter

import (
	"fmt"
	"os"
	"strings"
	"time"

	systemd "github.com/coreos/go-systemd/v22/dbus"
	dbus "github.com/godbus/dbus/v5"
	"github.com/golang/glog"

	"github.com/yandex-cloud/k8s-csi-s3/pkg/s3"
)

const (
	goofysCmd    = "goofys"
)

// Implements Mounter
type goofysMounter struct {
	meta            *s3.FSMeta
	endpoint        string
	region          string
	accessKeyID     string
	secretAccessKey string
}

func newGoofysMounter(meta *s3.FSMeta, cfg *s3.Config) (Mounter, error) {
	return &goofysMounter{
		meta:            meta,
		endpoint:        cfg.Endpoint,
		region:          cfg.Region,
		accessKeyID:     cfg.AccessKeyID,
		secretAccessKey: cfg.SecretAccessKey,
	}, nil
}

func (goofys *goofysMounter) CopyBinary(from, to string) error {
	st, err := os.Stat(from)
	if err != nil {
		return fmt.Errorf("Failed to stat %s: %v", from, err)
	}
	st2, err := os.Stat(to)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("Failed to stat %s: %v", to, err)
	}
	if err != nil || st2.Size() != st.Size() || st2.ModTime() != st.ModTime() {
		if err == nil {
			// remove the file first to not hit "text file busy" errors
			err = os.Remove(to)
			if err != nil {
				return fmt.Errorf("Error removing %s to update it: %v", to, err)
			}
		}
		bin, err := os.ReadFile(from)
		if err != nil {
			return fmt.Errorf("Error copying %s to %s: %v", from, to, err)
		}
		err = os.WriteFile(to, bin, 0755)
		if err != nil {
			return fmt.Errorf("Error copying %s to %s: %v", from, to, err)
		}
		err = os.Chtimes(to, st.ModTime(), st.ModTime())
		if err != nil {
			return fmt.Errorf("Error copying %s to %s: %v", from, to, err)
		}
	}
	return nil
}

func (goofys *goofysMounter) MountDirect(target string, args []string) error {
	args = append([]string{
		"--endpoint", goofys.endpoint,
		"-o", "allow_other",
	}, args...)
	envs := []string{
		"AWS_ACCESS_KEY_ID=" + goofys.accessKeyID,
		"AWS_SECRET_ACCESS_KEY=" + goofys.secretAccessKey,
	}
	return fuseMount(target, goofysCmd, args, envs)
}



func (goofys *goofysMounter) Mount(target, volumeID string) error {
	fullPath := fmt.Sprintf("%s:%s", goofys.meta.BucketName, goofys.meta.Prefix)
	var args []string
	if goofys.region != "" {
		args = append(args, "--region", goofys.region)
	}
	args = append(
		args,
		"--uid", "65534", // nobody. drop root privileges
		"--gid", "65534", // nogroup
	)
	var unsafeArgs []string
	useSystemd := true
	for i := 0; i < len(goofys.meta.MountOptions); i++ {
		opt := goofys.meta.MountOptions[i]
		if opt == "--no-systemd" {
			useSystemd = false
		} else if len(opt) > 0 && opt[0] == '-' {
			// Remove unsafe options
			s := 1
			if len(opt) > 1 && opt[1] == '-' {
				s++
			}
			key := opt[s:]
			e := strings.Index(opt, "=")
			if e >= 0 {
				key = opt[s:e]
			}
			if key == "log-file" || key == "shared-config" || key == "cache" {
				// Skip options accessing local FS
				unsafeArgs = append(unsafeArgs, opt)
				i++
				if i < len(goofys.meta.MountOptions) {
					unsafeArgs = append(unsafeArgs, goofys.meta.MountOptions[i])
				}
			} else if key != "" {
				args = append(args, opt)
			}
		} else if len(opt) > 0 {
			args = append(args, opt)
		}
	}
	if !useSystemd {
		// Unsafe options are allowed when running inside the container
		args = append(args, unsafeArgs...)
	}
	args = append(args, fullPath, target)
	// Try to start goofys using systemd so it doesn't get killed when the container exits
	if !useSystemd {
		return goofys.MountDirect(target, args)
	}
	conn, err := systemd.New()
	if err != nil {
		glog.Errorf("Failed to connect to systemd dbus service: %v, starting goofys directly", err)
		return goofys.MountDirect(target, args)
	}
	defer conn.Close()
	// systemd is present
	if err = goofys.CopyBinary("/usr/bin/goofys", "/csi/goofys"); err != nil {
		return err
	}
	pluginDir := os.Getenv("PLUGIN_DIR")
	if pluginDir == "" {
		pluginDir = "/var/lib/kubelet/plugins/ru.yandex.s3.csi"
	}
	args = append([]string{pluginDir+"/goofys", "-f", "-o", "allow_other", "--endpoint", goofys.endpoint}, args...)
	glog.Info("Starting goofys using systemd: "+strings.Join(args, " "))
	unitName := "goofys-"+systemd.PathBusEscape(volumeID)+".service"
	newProps := []systemd.Property{
		systemd.Property{
			Name: "Description",
			Value: dbus.MakeVariant("goofys mount for Kubernetes volume "+volumeID),
		},
		systemd.PropExecStart(args, false),
		systemd.Property{
			Name: "Environment",
			Value: dbus.MakeVariant([]string{ "AWS_ACCESS_KEY_ID="+goofys.accessKeyID, "AWS_SECRET_ACCESS_KEY="+goofys.secretAccessKey }),
		},
		systemd.Property{
			Name: "CollectMode",
			Value: dbus.MakeVariant("inactive-or-failed"),
		},
	}
	unitProps, err := conn.GetAllProperties(unitName)
	if err == nil {
		// Unit already exists
		if s, ok := unitProps["ActiveState"].(string); ok && (s == "active" || s == "activating" || s == "reloading") {
			// Unit is already active
			curPath := ""
			prevExec, ok := unitProps["ExecStart"].([][]interface{})
			if ok && len(prevExec) > 0 && len(prevExec[0]) >= 2 {
				execArgs, ok := prevExec[0][1].([]string)
				if ok && len(execArgs) >= 2 {
					curPath = execArgs[len(execArgs)-1]
				}
			}
			if curPath != target {
				// FIXME This may mean that the same bucket&path are used for multiple PVs. Support it somehow
				return fmt.Errorf(
					"goofys for volume %v is already mounted on host, but"+
					" in a different directory. We want %v, but it's in %v",
					volumeID, target, curPath,
				)
			}
			// Already mounted at right location, wait for mount
			return waitForMount(target, 30*time.Second)
		} else {
			// Stop and garbage collect the unit if automatic collection didn't work for some reason
			conn.StopUnit(unitName, "replace", nil)
			conn.ResetFailedUnit(unitName)
		}
	}
	unitPath := "/run/systemd/system/" + unitName + ".d"
	err = os.MkdirAll(unitPath, 0755)
	if err != nil {
		return fmt.Errorf("Error creating directory %s: %v", unitPath, err)
	}
	// force & lazy unmount to cleanup possibly dead mountpoints
	err = os.WriteFile(
		unitPath+"/50-ExecStopPost.conf",
		[]byte("[Service]\nExecStopPost=/bin/umount -f -l "+target+"\n"),
		0600,
	)
	if err != nil {
		return fmt.Errorf("Error writing %v/50-ExecStopPost.conf: %v", unitPath, err)
	}
	_, err = conn.StartTransientUnit(unitName, "replace", newProps, nil)
	if err != nil {
		return fmt.Errorf("Error starting systemd unit %s on host: %v", unitName, err)
	}
	return waitForMount(target, 30*time.Second)
}
