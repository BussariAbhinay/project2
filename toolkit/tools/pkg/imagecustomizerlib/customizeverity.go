// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package imagecustomizerlib

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/microsoft/CBL-Mariner/toolkit/tools/imagecustomizerapi"
	"github.com/microsoft/CBL-Mariner/toolkit/tools/imagegen/diskutils"
	"github.com/microsoft/CBL-Mariner/toolkit/tools/internal/file"
	"github.com/microsoft/CBL-Mariner/toolkit/tools/internal/safechroot"
	"github.com/microsoft/CBL-Mariner/toolkit/tools/internal/shell"
)

func enableVerityPartition(imageChroot *safechroot.Chroot) error {
	var err error

	// Integrate systemd veritysetup dracut module into initramfs img.
	systemdVerityDracutModule := "systemd-veritysetup"
	err = buildDracutModule(systemdVerityDracutModule, imageChroot)
	if err != nil {
		return err
	}

	// Update mariner config file with the new generated initramfs file.
	err = updateMarinerCfgWithInitramfs(imageChroot)
	if err != nil {
		return err
	}

	return nil
}

func buildDracutModule(dracutModuleName string, imageChroot *safechroot.Chroot) error {
	var err error

	listKernels := func() ([]string, error) {
		var kernels []string
		// Use RootDir to get the path on the host OS
		bootDir := filepath.Join(imageChroot.RootDir(), "boot")
		files, err := filepath.Glob(filepath.Join(bootDir, "vmlinuz-*"))
		if err != nil {
			return nil, err
		}
		for _, file := range files {
			kernels = append(kernels, filepath.Base(file))
		}
		return kernels, nil
	}

	kernelFiles, err := listKernels()
	if err != nil {
		return fmt.Errorf("failed to list kernels: %w", err)
	}

	if len(kernelFiles) == 0 {
		return fmt.Errorf("no kernels found in chroot environment")
	}

	// Check if more than one kernel is found
	if len(kernelFiles) > 1 {
		return fmt.Errorf("multiple kernels found in chroot environment, expected only one")
	}

	// Extract the version from the kernel filename (e.g., vmlinuz-5.15.131.1-2.cm2 -> 5.15.131.1-2.cm2)
	kernelVersion := strings.TrimPrefix(kernelFiles[0], "vmlinuz-")

	err = imageChroot.Run(func() error {
		// TODO: Config Dracut module systemd-veritysetup - task 6421.
		err = shell.ExecuteLiveWithErr(1, "dracut", "-f", "--kver", kernelVersion, "-a", dracutModuleName)
		return err
	})
	if err != nil {
		return fmt.Errorf("failed to build dracut module - (%s):\n%w", dracutModuleName, err)
	}

	return nil
}

func updateMarinerCfgWithInitramfs(imageChroot *safechroot.Chroot) error {
	var err error

	initramfsPath := filepath.Join(imageChroot.RootDir(), "boot/initramfs-*")
	// Fetch the initramfs file name.
	var initramfsFiles []string
	initramfsFiles, err = filepath.Glob(initramfsPath)
	if err != nil {
		return fmt.Errorf("failed to list initramfs file: %w", err)
	}

	// Ensure an initramfs file is found
	if len(initramfsFiles) != 1 {
		return fmt.Errorf("expected one initramfs file, but found %d", len(initramfsFiles))
	}

	newInitramfs := filepath.Base(initramfsFiles[0])

	cfgPath := filepath.Join(imageChroot.RootDir(), "boot/mariner.cfg")

	lines, err := file.ReadLines(cfgPath)
	if err != nil {
		return fmt.Errorf("failed to read mariner.cfg: %w", err)
	}

	// Update lines to reference the new initramfs
	for i, line := range lines {
		if strings.HasPrefix(line, "mariner_initrd=") {
			lines[i] = "mariner_initrd=" + newInitramfs
		}
	}
	// Write the updated lines back to mariner.cfg using the internal method
	err = file.WriteLines(lines, cfgPath)
	if err != nil {
		return fmt.Errorf("failed to write to mariner.cfg: %w", err)
	}

	return nil
}

func updateGrubConfig(dataPartitionIdType imagecustomizerapi.IdType, dataPartitionId string,
	hashPartitionIdType imagecustomizerapi.IdType, hashPartitionId string, rootHash string, grubCfgFullPath string,
) error {
	var err error

	// Format the dataPartitionId and hashPartitionId using the helper function.
	formattedDataPartition, err := systemdFormatPartitionId(dataPartitionIdType, dataPartitionId)
	if err != nil {
		return err
	}
	formattedHashPartition, err := systemdFormatPartitionId(hashPartitionIdType, hashPartitionId)
	if err != nil {
		return err
	}

	newArgs := fmt.Sprintf(
		"rd.systemd.verity=1 roothash=%s systemd.verity_root_data=%s systemd.verity_root_hash=%s systemd.verity_root_options=panic-on-corruption",
		rootHash, formattedDataPartition, formattedHashPartition,
	)

	// Read grub.cfg using the internal method
	lines, err := file.ReadLines(grubCfgFullPath)
	if err != nil {
		return fmt.Errorf("failed to read grub config: %v", err)
	}

	var updatedLines []string
	for _, line := range lines {
		trimmedLine := strings.TrimSpace(line)
		if strings.HasPrefix(trimmedLine, "linux ") {
			// Append new arguments to the line that starts with "linux"
			line += " " + newArgs
		}
		if strings.HasPrefix(trimmedLine, "set rootdevice=PARTUUID=") {
			// Replace the root device line with the new root device. TODO: add supported type 'user'
			line = "set rootdevice=/dev/mapper/root"
		}
		updatedLines = append(updatedLines, line)
	}

	err = file.WriteLines(updatedLines, grubCfgFullPath)
	if err != nil {
		return fmt.Errorf("failed to write updated grub config: %v", err)
	}

	return nil
}

// idToPartitionBlockDevicePath returns the block device path for a given idType and id.
func idToPartitionBlockDevicePath(idType imagecustomizerapi.IdType, id string, nbdDevice string, diskPartitions []diskutils.PartitionInfo) (string, error) {
	// Iterate over each partition to find the matching id.
	for _, partition := range diskPartitions {
		switch idType {
		case imagecustomizerapi.IdTypePartlabel:
			if partition.PartLabel == id {
				return partition.Path, nil
			}
		case imagecustomizerapi.IdTypeUuid:
			if partition.Uuid == id {
				return partition.Path, nil
			}
		case imagecustomizerapi.IdTypePartuuid:
			if partition.PartUuid == id {
				return partition.Path, nil
			}
		default:
			return "", fmt.Errorf("invalid idType provided (%s)", string(idType))
		}
	}

	// If no partition is found with the given id.
	return "", fmt.Errorf("no partition found for %s: %s", idType, id)
}

// systemdFormatPartitionId formats the partition ID based on the ID type following systemd dm-verity style.
func systemdFormatPartitionId(idType imagecustomizerapi.IdType, id string) (string, error) {
	switch idType {
	case imagecustomizerapi.IdTypePartlabel, imagecustomizerapi.IdTypeUuid, imagecustomizerapi.IdTypePartuuid:
		return fmt.Sprintf("%s=%s", strings.ToUpper(string(idType)), id), nil
	default:
		return "", fmt.Errorf("invalid idType provided (%s)", string(idType))
	}
}

// findFreeNBDDevice finds the first available NBD device.
func findFreeNBDDevice() (string, error) {
	files, err := filepath.Glob("/sys/class/block/nbd*")
	if err != nil {
		return "", err
	}

	for _, file := range files {
		// Check if the pid file exists. If it does not exist, the device is likely free.
		pidFile := filepath.Join(file, "pid")
		if _, err := os.Stat(pidFile); os.IsNotExist(err) {
			return "/dev/" + filepath.Base(file), nil
		}
	}

	return "", fmt.Errorf("no free nbd devices available")
}
