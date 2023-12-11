// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package imagecustomizerlib

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"

	"github.com/microsoft/CBL-Mariner/toolkit/tools/imagecustomizerapi"
	"github.com/microsoft/CBL-Mariner/toolkit/tools/internal/file"
	"github.com/microsoft/CBL-Mariner/toolkit/tools/internal/logger"
	"github.com/microsoft/CBL-Mariner/toolkit/tools/internal/shell"
)

const (
	tmpParitionDirName = "tmppartition"

	BaseImageName                = "image.raw"
	PartitionCustomizedImageName = "image2.raw"
)

var (
	// Version specifies the version of the Mariner Image Customizer tool.
	// The value of this string is inserted during compilation via a linker flag.
	ToolVersion = ""
)

func CustomizeImageWithConfigFile(buildDir string, configFile string, imageFile string,
	rpmsSources []string, outputImageFile string, outputImageFormat string,
	useBaseImageRpmRepos bool,
) error {
	var err error

	var config imagecustomizerapi.Config
	err = imagecustomizerapi.UnmarshalYamlFile(configFile, &config)
	if err != nil {
		return err
	}

	baseConfigPath, _ := filepath.Split(configFile)

	absBaseConfigPath, err := filepath.Abs(baseConfigPath)
	if err != nil {
		return fmt.Errorf("failed to get absolute path of config file directory:\n%w", err)
	}

	err = CustomizeImage(buildDir, absBaseConfigPath, &config, imageFile, rpmsSources, outputImageFile, outputImageFormat,
		useBaseImageRpmRepos)
	if err != nil {
		return err
	}

	return nil
}

func CustomizeImage(buildDir string, baseConfigPath string, config *imagecustomizerapi.Config, imageFile string,
	rpmsSources []string, outputImageFile string, outputImageFormat string, useBaseImageRpmRepos bool,
) error {
	var err error

	// Validate 'outputImageFormat' value.
	qemuOutputImageFormat, err := toQemuImageFormat(outputImageFormat)
	if err != nil {
		return err
	}

	// Validate config.
	err = validateConfig(baseConfigPath, config)
	if err != nil {
		return fmt.Errorf("invalid image config:\n%w", err)
	}

	// Normalize 'buildDir' path.
	buildDirAbs, err := filepath.Abs(buildDir)
	if err != nil {
		return err
	}

	// Create 'buildDir' directory.
	err = os.MkdirAll(buildDirAbs, os.ModePerm)
	if err != nil {
		return err
	}

	// Convert image file to raw format, so that a kernel loop device can be used to make changes to the image.
	buildImageFile := filepath.Join(buildDirAbs, BaseImageName)

	logger.Log.Infof("Mounting base image: %s", buildImageFile)
	err = shell.ExecuteLiveWithErr(1, "qemu-img", "convert", "-O", "raw", imageFile, buildImageFile)
	if err != nil {
		return fmt.Errorf("failed to convert image file to raw format:\n%w", err)
	}

	// Customize the partitions.
	buildImageFile, err = customizePartitions(buildDirAbs, baseConfigPath, config, buildImageFile)
	if err != nil {
		return err
	}

	// Customize the raw image file.
	err = customizeImageHelper(buildDirAbs, baseConfigPath, config, buildImageFile, rpmsSources, useBaseImageRpmRepos)
	if err != nil {
		return err
	}

	if config.SystemConfig.Verity.IsSet() {
		// Customize image for dm-verity, setting up verity metadata and security features.
		err = customizeVerityImageHelper(buildDirAbs, baseConfigPath, config, buildImageFile, rpmsSources, useBaseImageRpmRepos)
		if err != nil {
			return err
		}
	}

	// Create final output image file.
	logger.Log.Infof("Writing: %s", outputImageFile)

	outDir := filepath.Dir(outputImageFile)
	os.MkdirAll(outDir, os.ModePerm)

	err = shell.ExecuteLiveWithErr(1, "qemu-img", "convert", "-O", qemuOutputImageFormat, buildImageFile, outputImageFile)
	if err != nil {
		return fmt.Errorf("failed to convert image file to format: %s:\n%w", outputImageFormat, err)
	}

	logger.Log.Infof("Success!")

	return nil
}

func toQemuImageFormat(imageFormat string) (string, error) {
	switch imageFormat {
	case "vhd":
		return "vpc", nil

	case "vhdx", "raw", "qcow2":
		return imageFormat, nil

	default:
		return "", fmt.Errorf("unsupported image format (supported: vhd, vhdx, raw, qcow2): %s", imageFormat)
	}
}

func validateConfig(baseConfigPath string, config *imagecustomizerapi.Config) error {
	var err error

	err = validateSystemConfig(baseConfigPath, &config.SystemConfig)
	if err != nil {
		return err
	}

	return nil
}

func validateSystemConfig(baseConfigPath string, config *imagecustomizerapi.SystemConfig) error {
	var err error

	for sourceFile := range config.AdditionalFiles {
		sourceFileFullPath := filepath.Join(baseConfigPath, sourceFile)
		isFile, err := file.IsFile(sourceFileFullPath)
		if err != nil {
			return fmt.Errorf("invalid AdditionalFiles source file (%s):\n%w", sourceFile, err)
		}

		if !isFile {
			return fmt.Errorf("invalid AdditionalFiles source file (%s): not a file", sourceFile)
		}
	}

	for i, script := range config.PostInstallScripts {
		err = validateScript(baseConfigPath, &script)
		if err != nil {
			return fmt.Errorf("invalid PostInstallScripts item at index %d: %w", i, err)
		}
	}

	for i, script := range config.FinalizeImageScripts {
		err = validateScript(baseConfigPath, &script)
		if err != nil {
			return fmt.Errorf("invalid FinalizeImageScripts item at index %d: %w", i, err)
		}
	}

	return nil
}

func validateScript(baseConfigPath string, script *imagecustomizerapi.Script) error {
	// Ensure that install scripts sit under the config file's parent directory.
	// This allows the install script to be run in the chroot environment by bind mounting the config directory.
	if !filepath.IsLocal(script.Path) {
		return fmt.Errorf("install script (%s) is not under config directory (%s)", script.Path, baseConfigPath)
	}

	// Verify that the file exists.
	fullPath := filepath.Join(baseConfigPath, script.Path)

	scriptStat, err := os.Stat(fullPath)
	if err != nil {
		return fmt.Errorf("couldn't read install script (%s):\n%w", script.Path, err)
	}

	// Verify that the file has an executable bit set.
	if scriptStat.Mode()&0111 == 0 {
		return fmt.Errorf("install script (%s) does not have executable bit set", script.Path)
	}

	return nil
}

func customizeImageHelper(buildDir string, baseConfigPath string, config *imagecustomizerapi.Config,
	buildImageFile string, rpmsSources []string, useBaseImageRpmRepos bool,
) error {
	imageConnection, err := connectToExistingImage(buildImageFile, buildDir, "imageroot")
	if err != nil {
		return err
	}
	defer imageConnection.Close()

	// Do the actual customizations.
	err = doCustomizations(buildDir, baseConfigPath, config, imageConnection.Chroot(), rpmsSources, useBaseImageRpmRepos)
	if err != nil {
		return err
	}

	err = imageConnection.CleanClose()
	if err != nil {
		return err
	}

	return nil
}

func customizeVerityImageHelper(buildDir string, baseConfigPath string, config *imagecustomizerapi.Config,
	buildImageFile string, rpmsSources []string, useBaseImageRpmRepos bool,
) error {
	var err error

	// Connect the disk image to an NBD device using qemu-nbd
	// Find a free NBD device
	nbdDevice, err := findFreeNBDDevice()
	if err != nil {
		return fmt.Errorf("failed to find a free nbd device: %v", err)
	}

	_, _, err = shell.Execute("sudo", "qemu-nbd", "-c", nbdDevice, "-f", "raw", buildImageFile)
	if err != nil {
		return fmt.Errorf("failed to connect nbd %s to image %s: %s", nbdDevice, buildImageFile, err)
	}
	defer func() {
		// Disconnect the NBD device when the function returns
		_, _, err = shell.Execute("sudo", "qemu-nbd", "-d", nbdDevice)
		if err != nil {
			return
		}
	}()

	// Resolve DataPartition and HashPartition if they are specified as PARTUUID or PARTLABEL
	resolvedDataPartition, err := findDeviceByUUIDOrLabel(config.SystemConfig.Verity.DataPartition.Id)
	if err != nil {
		return fmt.Errorf("failed to resolve verity device %s: %v", config.SystemConfig.Verity.DataPartition.Id, err)
	}
	resolvedHashPartition, err := findDeviceByUUIDOrLabel(config.SystemConfig.Verity.HashPartition.Id)
	if err != nil {
		return fmt.Errorf("failed to resolve hash device %s: %v", config.SystemConfig.Verity.HashPartition.Id, err)
	}

	// Convert the system config devices to nbd partitions
	nbdDataPartition, err := convertToNbdDevicePath(nbdDevice, resolvedDataPartition)
	if err != nil {
		return err
	}
	nbdHashPartition, err := convertToNbdDevicePath(nbdDevice, resolvedHashPartition)
	if err != nil {
		return err
	}

	// Extract root hash using regular expressions
	verityOutput, _, err := shell.Execute("sudo", "veritysetup", "format", nbdDataPartition, nbdHashPartition)
	if err != nil {
		return fmt.Errorf("failed to calculate root hash:\n%w", err)
	}

	var rootHash string
	rootHashRegex := regexp.MustCompile(`Root hash:\s+([0-9a-fA-F]+)`)

	rootHashMatches := rootHashRegex.FindStringSubmatch(verityOutput)
	if len(rootHashMatches) <= 1 {
		return fmt.Errorf("failed to parse root hash from veritysetup output")
	}
	rootHash = rootHashMatches[1]

	resolvedBootPartition, err := findDeviceByUUIDOrLabel(config.SystemConfig.Verity.BootPartition.Id)
	if err != nil {
		return fmt.Errorf("failed to resolve boot device %s: %v", config.SystemConfig.Verity.BootPartition.Id, err)
	}

	nbdBootPartition, err := convertToNbdDevicePath(nbdDevice, resolvedBootPartition)
	if err != nil {
		return err
	}

	// Create a directory for mounting the boot partition
	bootMountDir := filepath.Join(buildDir, "/mnt/boot_partition")
		if err := os.MkdirAll(bootMountDir, 0755); err != nil {
			return fmt.Errorf("failed to create mount directory %s: %v", bootMountDir, err)
		}
		defer func() {
			// Cleanup: Unmount and remove the directory when the function returns
			if err := exec.Command("sudo", "umount", bootMountDir).Run(); err != nil {
				fmt.Printf("Warning: failed to unmount %s: %v\n", bootMountDir, err)
			}
			if err := os.Remove(bootMountDir); err != nil {
				fmt.Printf("Warning: failed to remove %s: %v\n", bootMountDir, err)
			}
		}()

		_, _, err = shell.Execute("sudo", "mount", nbdBootPartition, bootMountDir)
		if err != nil {
			return err
		}

	// Update grub configuration
	err = updateGrubConfig(resolvedDataPartition, resolvedHashPartition, rootHash, config.SystemConfig.Verity.VerityErrorBehavior, bootMountDir)
	if err != nil {
		return err
	}

	return nil
}
