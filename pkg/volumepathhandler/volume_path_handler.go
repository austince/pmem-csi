/*
Copyright 2018 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package volumepathhandler

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"

	"k8s.io/utils/exec"
	"k8s.io/utils/mount"

	"k8s.io/apimachinery/pkg/types"

	pmemlog "github.com/intel/pmem-csi/pkg/logger"
)

const (
	losetupPath           = "losetup"
	statPath              = "stat"
	ErrDeviceNotFound     = "device not found"
	ErrDeviceNotSupported = "device not supported"
)

// BlockVolumePathHandler defines a set of operations for handling block volume-related operations
type BlockVolumePathHandler interface {
	// MapDevice creates a symbolic link to block device under specified map path
	MapDevice(ctx context.Context, devicePath string, mapPath string, linkName string, bindMount bool) error
	// UnmapDevice removes a symbolic link to block device under specified map path
	UnmapDevice(ctx context.Context, mapPath string, linkName string, bindMount bool) error
	// RemovePath removes a file or directory on specified map path
	RemoveMapPath(ctx context.Context, mapPath string) error
	// IsSymlinkExist retruns true if specified symbolic link exists
	IsSymlinkExist(ctx context.Context, mapPath string) (bool, error)
	// IsDeviceBindMountExist retruns true if specified bind mount exists
	IsDeviceBindMountExist(ctx context.Context, mapPath string) (bool, error)
	// GetDeviceBindMountRefs searches bind mounts under global map path
	GetDeviceBindMountRefs(ctx context.Context, devPath string, mapPath string) ([]string, error)
	// FindGlobalMapPathUUIDFromPod finds {pod uuid} symbolic link under globalMapPath
	// corresponding to map path symlink, and then return global map path with pod uuid.
	FindGlobalMapPathUUIDFromPod(ctx context.Context, pluginDir, mapPath string, podUID types.UID) (string, error)
	// AttachFileDevice takes a path to a regular file and makes it available as an
	// attached block device.
	AttachFileDevice(ctx context.Context, path string) (string, error)
	// DetachFileDevice takes a path to the attached block device and
	// detach it from block device.
	DetachFileDevice(ctx context.Context, path string) error
	// GetLoopDevice returns the full path to the loop device associated with the given path.
	GetLoopDevice(ctx context.Context, path string) (string, error)
}

// NewBlockVolumePathHandler returns a new instance of BlockVolumeHandler.
func NewBlockVolumePathHandler() BlockVolumePathHandler {
	var volumePathHandler VolumePathHandler
	return volumePathHandler
}

// VolumePathHandler is path related operation handlers for block volume
type VolumePathHandler struct {
}

// MapDevice creates a symbolic link to block device under specified map path
func (v VolumePathHandler) MapDevice(ctx context.Context, devicePath string, mapPath string, linkName string, bindMount bool) error {
	ctx, logger := pmemlog.WithName(ctx, "MapDevice")

	// Example of global map path:
	//   globalMapPath/linkName: plugins/kubernetes.io/{PluginName}/{DefaultKubeletVolumeDevicesDirName}/{volumePluginDependentPath}/{podUid}
	//   linkName: {podUid}
	//
	// Example of pod device map path:
	//   podDeviceMapPath/linkName: pods/{podUid}/{DefaultKubeletVolumeDevicesDirName}/{escapeQualifiedPluginName}/{volumeName}
	//   linkName: {volumeName}
	if len(devicePath) == 0 {
		return fmt.Errorf("failed to map device to map path. devicePath is empty")
	}
	if len(mapPath) == 0 {
		return fmt.Errorf("failed to map device to map path. mapPath is empty")
	}
	if !filepath.IsAbs(mapPath) {
		return fmt.Errorf("the map path should be absolute: map path: %s", mapPath)
	}
	logger.V(5).Info("Setting up",
		"device-path", devicePath,
		"map-path", mapPath,
		"link-name", linkName,
	)

	// Check and create mapPath
	_, err := os.Stat(mapPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("cannot validate map path: %s: %v", mapPath, err)
	}
	if err = os.MkdirAll(mapPath, 0750); err != nil {
		return fmt.Errorf("failed to mkdir %s: %v", mapPath, err)
	}

	if bindMount {
		return mapBindMountDevice(ctx, v, devicePath, mapPath, linkName)
	}
	return mapSymlinkDevice(v, devicePath, mapPath, linkName)
}

func mapBindMountDevice(ctx context.Context, v VolumePathHandler, devicePath string, mapPath string, linkName string) error {
	ctx, logger := pmemlog.WithName(ctx, "mapBindMountDevice")

	// Check bind mount exists
	linkPath := filepath.Join(mapPath, string(linkName))

	file, err := os.Stat(linkPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("failed to stat file %s: %v", linkPath, err)
		}

		// Create file
		newFile, err := os.OpenFile(linkPath, os.O_CREATE|os.O_RDWR, 0750)
		if err != nil {
			return fmt.Errorf("failed to open file %s: %v", linkPath, err)
		}
		if err := newFile.Close(); err != nil {
			return fmt.Errorf("failed to close file %s: %v", linkPath, err)
		}
	} else {
		// Check if device file
		// TODO: Need to check if this device file is actually the expected bind mount
		if file.Mode()&os.ModeDevice == os.ModeDevice {
			logger.V(2).Info("Warning: Map skipped because bind mount already exist on the path", "link-path", linkPath)
			return nil
		}

		logger.V(2).Info("Warning: file already exists but is not mounted, skip creating file", "link-path", linkPath)
	}

	// Bind mount file
	mounter := &mount.SafeFormatAndMount{Interface: mount.New(""), Exec: exec.New()}
	if err := mounter.Mount(devicePath, linkPath, "" /* fsType */, []string{"bind"}); err != nil {
		return fmt.Errorf("failed to bind mount devicePath: %s to linkPath %s: %v", devicePath, linkPath, err)
	}

	return nil
}

func mapSymlinkDevice(v VolumePathHandler, devicePath string, mapPath string, linkName string) error {
	// Remove old symbolic link(or file) then create new one.
	// This should be done because current symbolic link is
	// stale across node reboot.
	linkPath := filepath.Join(mapPath, string(linkName))
	if err := os.Remove(linkPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove file %s: %v", linkPath, err)
	}
	return os.Symlink(devicePath, linkPath)
}

// UnmapDevice removes a symbolic link associated to block device under specified map path
func (v VolumePathHandler) UnmapDevice(ctx context.Context, mapPath string, linkName string, bindMount bool) error {
	ctx, logger := pmemlog.WithName(ctx, "UnmapDevice")
	if len(mapPath) == 0 {
		return fmt.Errorf("failed to unmap device from map path. mapPath is empty")
	}
	logger.V(5).Info("Unmapping", "map-path", mapPath, "link-path", linkName)

	if bindMount {
		return unmapBindMountDevice(ctx, v, mapPath, linkName)
	}
	return unmapSymlinkDevice(ctx, v, mapPath, linkName)
}

func unmapBindMountDevice(ctx context.Context, v VolumePathHandler, mapPath string, linkName string) error {
	ctx, logger := pmemlog.WithName(ctx, "unmapBindMountDevice")

	// Check bind mount exists
	linkPath := filepath.Join(mapPath, string(linkName))
	if isMountExist, checkErr := v.IsDeviceBindMountExist(ctx, linkPath); checkErr != nil {
		return checkErr
	} else if !isMountExist {
		logger.V(2).Info("Warning: Unmap skipped because bind mount does not exist on the path", "link-path", linkPath)

		// Check if linkPath still exists
		if _, err := os.Stat(linkPath); err != nil {
			if !os.IsNotExist(err) {
				return fmt.Errorf("failed to check if path %s exists: %v", linkPath, err)
			}
			// linkPath has already been removed
			return nil
		}
		// Remove file
		if err := os.Remove(linkPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("failed to remove file %s: %v", linkPath, err)
		}
		return nil
	}

	// Unmount file
	mounter := &mount.SafeFormatAndMount{Interface: mount.New(""), Exec: exec.New()}
	if err := mounter.Unmount(linkPath); err != nil {
		return fmt.Errorf("failed to unmount linkPath %s: %v", linkPath, err)
	}

	// Remove file
	if err := os.Remove(linkPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove file %s: %v", linkPath, err)
	}

	return nil
}

func unmapSymlinkDevice(ctx context.Context, v VolumePathHandler, mapPath string, linkName string) error {
	ctx, logger := pmemlog.WithName(ctx, "unmapSymlinkDevice")

	// Check symbolic link exists
	linkPath := filepath.Join(mapPath, string(linkName))
	if islinkExist, checkErr := v.IsSymlinkExist(ctx, linkPath); checkErr != nil {
		return checkErr
	} else if !islinkExist {
		logger.V(2).Info("Warning: Unmap skipped because symlink does not exist on the path", "link-path", linkPath)
		return nil
	}
	return os.Remove(linkPath)
}

// RemoveMapPath removes a file or directory on specified map path
func (v VolumePathHandler) RemoveMapPath(ctx context.Context, mapPath string) error {
	ctx, logger := pmemlog.WithName(ctx, "RemoveMapPath")
	if len(mapPath) == 0 {
		return fmt.Errorf("failed to remove map path. mapPath is empty")
	}
	logger.V(5).Info("Removing", "map-path", mapPath)
	err := os.RemoveAll(mapPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove directory %s: %v", mapPath, err)
	}
	return nil
}

// IsSymlinkExist returns true if specified file exists and the type is symbolik link.
// If file doesn't exist, or file exists but not symbolic link, return false with no error.
// On other cases, return false with error from Lstat().
func (v VolumePathHandler) IsSymlinkExist(ctx context.Context, mapPath string) (bool, error) {
	fi, err := os.Lstat(mapPath)
	if err != nil {
		// If file doesn't exist, return false and no error
		if os.IsNotExist(err) {
			return false, nil
		}
		// Return error from Lstat()
		return false, fmt.Errorf("failed to Lstat file %s: %v", mapPath, err)
	}
	// If file exits and it's symbolic link, return true and no error
	if fi.Mode()&os.ModeSymlink == os.ModeSymlink {
		return true, nil
	}
	// If file exits but it's not symbolic link, return fale and no error
	return false, nil
}

// IsDeviceBindMountExist returns true if specified file exists and the type is device.
// If file doesn't exist, or file exists but not device, return false with no error.
// On other cases, return false with error from Lstat().
func (v VolumePathHandler) IsDeviceBindMountExist(ctx context.Context, mapPath string) (bool, error) {
	fi, err := os.Lstat(mapPath)
	if err != nil {
		// If file doesn't exist, return false and no error
		if os.IsNotExist(err) {
			return false, nil
		}

		// Return error from Lstat()
		return false, fmt.Errorf("failed to Lstat file %s: %v", mapPath, err)
	}
	// If file exits and it's device, return true and no error
	if fi.Mode()&os.ModeDevice == os.ModeDevice {
		return true, nil
	}
	// If file exits but it's not device, return fale and no error
	return false, nil
}

// GetDeviceBindMountRefs searches bind mounts under global map path
func (v VolumePathHandler) GetDeviceBindMountRefs(ctx context.Context, devPath string, mapPath string) ([]string, error) {
	ctx, logger := pmemlog.WithName(ctx, "GetDeviceBindMountRefs")
	var refs []string
	files, err := ioutil.ReadDir(mapPath)
	if err != nil {
		return nil, fmt.Errorf("directory cannot read %v", err)
	}
	for _, file := range files {
		if file.Mode()&os.ModeDevice != os.ModeDevice {
			continue
		}
		filename := file.Name()
		// TODO: Might need to check if the file is actually linked to devPath
		refs = append(refs, filepath.Join(mapPath, filename))
	}
	logger.V(5).Info("Completed", "refs", refs)
	return refs, nil
}
