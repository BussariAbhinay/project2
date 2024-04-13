// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package imagecustomizerlib

import (
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/microsoft/azurelinux/toolkit/tools/imagecustomizerapi"
	"github.com/microsoft/azurelinux/toolkit/tools/imagegen/diskutils"
	"github.com/microsoft/azurelinux/toolkit/tools/internal/file"
	"github.com/microsoft/azurelinux/toolkit/tools/internal/logger"
	"github.com/microsoft/azurelinux/toolkit/tools/internal/safeloopback"
	"github.com/microsoft/azurelinux/toolkit/tools/internal/safemount"
	"github.com/microsoft/azurelinux/toolkit/tools/internal/shell"
)

const (
	tmpParitionDirName = "tmppartition"

	// supported input formats
	ImageFormatVhd   = "vhd"
	ImageFormatVhdx  = "vhdx"
	ImageFormatQCow2 = "qcow2"
	ImageFormatIso   = "iso"
	ImageFormatRaw   = "raw"

	// qemu-specific formats
	QemuFormatVpc = "vpc"

	BaseImageName                = "image.raw"
	PartitionCustomizedImageName = "image2.raw"
)

var (
	// Version specifies the version of the Azure Linux Image Customizer tool.
	// The value of this string is inserted during compilation via a linker flag.
	ToolVersion = ""
)

func CustomizeImageWithConfigFile(buildDir string, configFile string, imageFile string,
	rpmsSources []string, outputImageFile string, outputImageFormat string,
	outputSplitPartitionsFormat string, useBaseImageRpmRepos bool, enableShrinkFilesystems bool,
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
		outputSplitPartitionsFormat, useBaseImageRpmRepos, enableShrinkFilesystems)
	if err != nil {
		return err
	}

	return nil
}

type CommonParameters struct {
	buildDir    string
	buildDirAbs string

	inputImageFile string

	configPath                  string
	config                      *imagecustomizerapi.Config
	customizeOSPartitions       bool
	useBaseImageRpmRepos        bool
	rpmsSources                 []string
	enableShrinkFilesystems     bool
	outputSplitPartitionsFormat string

	rawImageFile string

	outputImageFormat     string
	qemuOutputImageFormat string
	outputImageFile       string
	outputImageDir        string
	outputImageBase       string

	isoBuilder *LiveOSIsoBuilder
}

func initCommonParameters(buildDir string,
	inputImageFile string,
	configPath string, config *imagecustomizerapi.Config,
	useBaseImageRpmRepos bool, rpmsSources []string, enableShrinkFilesystems bool, outputSplitPartitionsFormat string,
	outputImageFormat string, outputImageFile string) (*CommonParameters, error) {

	cp := &CommonParameters{}

	// working directories
	cp.buildDir = buildDir

	buildDirAbs, err := filepath.Abs(buildDir)
	if err != nil {
		return nil, err
	}

	cp.buildDirAbs = buildDirAbs

	err = os.MkdirAll(cp.buildDirAbs, os.ModePerm)
	if err != nil {
		return nil, err
	}

	// input
	cp.inputImageFile = inputImageFile

	// configuration
	cp.configPath = configPath
	cp.config = config
	cp.customizeOSPartitions = (config.Storage != nil) || (config.OS != nil)

	cp.useBaseImageRpmRepos = useBaseImageRpmRepos
	cp.rpmsSources = rpmsSources

	cp.enableShrinkFilesystems = enableShrinkFilesystems
	cp.outputSplitPartitionsFormat = outputSplitPartitionsFormat

	// writeable image
	cp.rawImageFile = filepath.Join(buildDirAbs, BaseImageName)

	// output
	cp.outputImageFormat = outputImageFormat
	cp.outputImageFile = outputImageFile
	cp.outputImageBase = strings.TrimSuffix(filepath.Base(outputImageFile), filepath.Ext(outputImageFile))
	cp.outputImageDir = filepath.Dir(outputImageFile)

	if cp.outputImageFormat != "" && cp.outputImageFormat != ImageFormatIso {
		cp.qemuOutputImageFormat, err = toQemuImageFormat(cp.outputImageFormat)
		if err != nil {
			return nil, err
		}
	}

	err = os.MkdirAll(cp.outputImageDir, os.ModePerm)
	if err != nil {
		return nil, err
	}

	return cp, nil
}

func CustomizeImage(buildDir string, baseConfigPath string, config *imagecustomizerapi.Config, imageFile string,
	rpmsSources []string, outputImageFile string, outputImageFormat string, outputSplitPartitionsFormat string,
	useBaseImageRpmRepos bool, enableShrinkFilesystems bool,
) error {
	var err error

	err = validateConfig(baseConfigPath, config, rpmsSources, useBaseImageRpmRepos)
	if err != nil {
		return fmt.Errorf("invalid image config:\n%w", err)
	}

	cp, err := initCommonParameters(buildDir, imageFile, baseConfigPath, config,
		useBaseImageRpmRepos, rpmsSources, enableShrinkFilesystems, outputSplitPartitionsFormat,
		outputImageFormat, outputImageFile)
	if err != nil {
		return fmt.Errorf("failed to initialize image customizer state:\n%w", err)
	}

	err = cp.convertInputImageToWriteableFormat()
	if err != nil {
		return fmt.Errorf("failed to convert input image to writeable raw image:\n%w", err)
	}
	defer func() {
		cleanupErr := file.RemoveFileIfExists(cp.rawImageFile)
		if cleanupErr != nil {
			if err != nil {
				err = fmt.Errorf("%w:\nfailed to clean-up (%s): %w", err, cp.rawImageFile, cleanupErr)
			} else {
				err = fmt.Errorf("failed to clean-up (%s): %w", cp.rawImageFile, cleanupErr)
			}
		}
	}()

	err = cp.customizeOSContents()
	if err != nil {
		return fmt.Errorf("failed to customize raw image:\n%w", err)
	}

	err = cp.convertWriteableFormatToOutputImage()
	if err != nil {
		return fmt.Errorf("failed to convert customized raw image to output format:\n%w", err)
	}

	logger.Log.Infof("Success!")

	return nil
}

func (cp *CommonParameters) convertInputImageToWriteableFormat() error {

	logger.Log.Debugf("---- dev ---- converting input image to raw disk")

	if filepath.Ext(cp.inputImageFile) == ".iso" {

		logger.Log.Debugf("---- dev ---- input image is iso. Expanding...")

		isoExpansionFolder, err := ioutil.TempDir(cp.buildDirAbs, "expanded-input-iso-")
		if err != nil {
			return fmt.Errorf("failed to create temporary iso expansion folder for iso:\n%w", err)
		}
		// clean-up
		// defer os.RemoveAll(isoExpansionFolder)

		err = copyIsoImageContentsToFolder(cp.buildDir, cp.inputImageFile, isoExpansionFolder)
		if err != nil {
			return fmt.Errorf("failed to expand input iso file:\n%w", err)
		}

		cp.isoBuilder, err = isoBuilderFromFolder(cp.buildDir, isoExpansionFolder)
		if err != nil {
			return fmt.Errorf("failed to load input iso artifacts:\n%w", err)
		}

		if cp.customizeOSPartitions {
			logger.Log.Debugf("---- dev ---- converting squashfs into a full writeable disk image...")
			err := cp.isoBuilder.createWriteableImageFromSquashfs(cp.buildDir, cp.rawImageFile)
			if err != nil {
				return fmt.Errorf("failed to create writeable image:\n%w", err)
			}
		}
	} else {
		logger.Log.Debugf("---- dev ---- converting input disk image into a full writeable disk image...")
		logger.Log.Infof("Creating raw base image: %s", cp.rawImageFile)
		err := shell.ExecuteLiveWithErr(1, "qemu-img", "convert", "-O", "raw", cp.inputImageFile, cp.rawImageFile)
		if err != nil {
			return fmt.Errorf("failed to convert image file to raw format:\n%w", err)
		}
	}

	return nil
}

func (cp *CommonParameters) customizeOSContents() error {

	logger.Log.Debugf("---- dev ---- customizing full disk image...")
	if !cp.customizeOSPartitions {
		logger.Log.Debugf("---- dev ---- skipping customizing full disk image...")
		return nil
	}

	// Customize the partitions.
	partitionsCustomized, newRawImageFile, err := customizePartitions(cp.buildDirAbs, cp.configPath, cp.config, cp.rawImageFile)
	if err != nil {
		return err
	}
	cp.rawImageFile = newRawImageFile

	// Customize the raw image file.
	err = customizeImageHelper(cp.buildDirAbs, cp.configPath, cp.config, cp.rawImageFile, cp.rpmsSources, cp.useBaseImageRpmRepos,
		partitionsCustomized)
	if err != nil {
		return err
	}

	// Shrink the filesystems.
	if cp.enableShrinkFilesystems {
		err = shrinkFilesystemsHelper(cp.rawImageFile)
		if err != nil {
			return fmt.Errorf("failed to shrink filesystems:\n%w", err)
		}
	}

	if cp.config.OS.Verity != nil {
		// Customize image for dm-verity, setting up verity metadata and security features.
		err = customizeVerityImageHelper(cp.buildDirAbs, cp.configPath, cp.config, cp.rawImageFile, cp.rpmsSources, cp.useBaseImageRpmRepos)
		if err != nil {
			return err
		}
	}

	// Check file systems for corruption.
	err = checkFileSystems(cp.rawImageFile)
	if err != nil {
		return fmt.Errorf("failed to check filesystems:\n%w", err)
	}

	// If outputSplitPartitionsFormat is specified, extract the partition files.
	if cp.outputSplitPartitionsFormat != "" {
		logger.Log.Infof("Extracting partition files")
		err = extractPartitionsHelper(cp.rawImageFile, cp.outputImageDir, cp.outputImageBase, cp.outputSplitPartitionsFormat)
		if err != nil {
			return err
		}
	}

	return nil
}

func (cp *CommonParameters) convertWriteableFormatToOutputImage() error {

	logger.Log.Debugf("---- dev ---- converting writeable full disk image into final image...")

	// Create final output image file if requested.
	switch cp.outputImageFormat {
	case ImageFormatVhd, ImageFormatVhdx, ImageFormatQCow2, ImageFormatRaw:
		logger.Log.Debugf("---- dev ---- creating the final full disk image...")
		logger.Log.Infof("Writing: %s", cp.outputImageFile)

		err := shell.ExecuteLiveWithErr(1, "qemu-img", "convert", "-O", cp.qemuOutputImageFormat, cp.rawImageFile, cp.outputImageFile)
		if err != nil {
			return fmt.Errorf("failed to convert image file to format: %s:\n%w", cp.outputImageFormat, err)
		}
	case ImageFormatIso:
		if cp.customizeOSPartitions {
			logger.Log.Debugf("---- dev ---- creating the final iso from customized raw image...")
			err := createLiveOSIsoImage(cp.buildDir, cp.configPath, cp.config.Iso, cp.rawImageFile, cp.outputImageDir, cp.outputImageBase)
			if err != nil {
				return err
			}
		} else {
			logger.Log.Debugf("---- dev ---- no squashfs customizations, customizing iso file system only...")
			err := cp.isoBuilder.recreateLiveOSIsoImage(cp.configPath, cp.config.Iso, cp.outputImageDir, cp.outputImageBase)
			if err != nil {
				return fmt.Errorf("failed to create LiveOS ISO:\n%w", err)
			}
		}
	}

	return nil
}

func toQemuImageFormat(imageFormat string) (string, error) {
	switch imageFormat {
	case ImageFormatVhd:
		return QemuFormatVpc, nil

	case ImageFormatVhdx, ImageFormatRaw, ImageFormatQCow2:
		return imageFormat, nil

	default:
		return "", fmt.Errorf("unsupported image format (supported: vhd, vhdx, raw, qcow2): %s", imageFormat)
	}
}

func validateConfig(baseConfigPath string, config *imagecustomizerapi.Config, rpmsSources []string,
	useBaseImageRpmRepos bool,
) error {
	// Note: This IsValid() check does duplicate the one in UnmarshalYamlFile().
	// But it is useful for functions that call CustomizeImage() directly. For example, test code.
	err := config.IsValid()
	if err != nil {
		return err
	}

	partitionsCustomized := hasPartitionCustomizations(config)

	err = validateIsoConfig(baseConfigPath, config.Iso)
	if err != nil {
		return err
	}

	err = validateSystemConfig(baseConfigPath, config.OS, rpmsSources, useBaseImageRpmRepos,
		partitionsCustomized)
	if err != nil {
		return err
	}

	return nil
}

func hasPartitionCustomizations(config *imagecustomizerapi.Config) bool {
	return config.Storage != nil
}

func validateAdditionalFiles(baseConfigPath string, additionalFiles imagecustomizerapi.AdditionalFilesMap) error {
	var aggregateErr error
	for sourceFile := range additionalFiles {
		sourceFileFullPath := file.GetAbsPathWithBase(baseConfigPath, sourceFile)
		isFile, err := file.IsFile(sourceFileFullPath)
		if err != nil {
			aggregateErr = errors.Join(aggregateErr, fmt.Errorf("invalid additionalFiles source file (%s):\n%w", sourceFile, err))
		}

		if !isFile {
			aggregateErr = errors.Join(aggregateErr, fmt.Errorf("invalid additionalFiles source file (%s): not a file", sourceFile))
		}
	}
	return aggregateErr
}

func validateIsoConfig(baseConfigPath string, config *imagecustomizerapi.Iso) error {
	if config == nil {
		return nil
	}

	err := validateAdditionalFiles(baseConfigPath, config.AdditionalFiles)
	if err != nil {
		return err
	}

	return nil
}

func validateSystemConfig(baseConfigPath string, config *imagecustomizerapi.OS,
	rpmsSources []string, useBaseImageRpmRepos bool, partitionsCustomized bool,
) error {
	var err error

	err = validatePackageLists(baseConfigPath, config, rpmsSources, useBaseImageRpmRepos, partitionsCustomized)
	if err != nil {
		return err
	}

	err = validateAdditionalFiles(baseConfigPath, config.AdditionalFiles)
	if err != nil {
		return err
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

func validatePackageLists(baseConfigPath string, config *imagecustomizerapi.OS, rpmsSources []string,
	useBaseImageRpmRepos bool, partitionsCustomized bool,
) error {
	allPackagesRemove, err := collectPackagesList(baseConfigPath, config.Packages.RemoveLists, config.Packages.Remove)
	if err != nil {
		return err
	}

	allPackagesInstall, err := collectPackagesList(baseConfigPath, config.Packages.InstallLists, config.Packages.Install)
	if err != nil {
		return err
	}

	allPackagesUpdate, err := collectPackagesList(baseConfigPath, config.Packages.UpdateLists, config.Packages.Update)
	if err != nil {
		return err
	}

	hasRpmSources := len(rpmsSources) > 0 || useBaseImageRpmRepos

	if !hasRpmSources {
		needRpmsSources := len(allPackagesInstall) > 0 || len(allPackagesUpdate) > 0 ||
			config.Packages.UpdateExistingPackages

		if needRpmsSources {
			return fmt.Errorf("have packages to install or update but no RPM sources were specified")
		} else if partitionsCustomized {
			return fmt.Errorf("partitions were customized so the initramfs package needs to be reinstalled but no RPM sources were specified")
		}
	}

	config.Packages.Remove = allPackagesRemove
	config.Packages.Install = allPackagesInstall
	config.Packages.Update = allPackagesUpdate

	config.Packages.RemoveLists = nil
	config.Packages.InstallLists = nil
	config.Packages.UpdateLists = nil

	return nil
}

func customizeImageHelper(buildDir string, baseConfigPath string, config *imagecustomizerapi.Config,
	rawImageFile string, rpmsSources []string, useBaseImageRpmRepos bool, partitionsCustomized bool,
) error {
	imageConnection, err := connectToExistingImage(rawImageFile, buildDir, "imageroot", true)
	if err != nil {
		return err
	}
	defer imageConnection.Close()

	// Do the actual customizations.
	err = doCustomizations(buildDir, baseConfigPath, config, imageConnection, rpmsSources,
		useBaseImageRpmRepos, partitionsCustomized)
	if err != nil {
		return err
	}

	err = imageConnection.CleanClose()
	if err != nil {
		return err
	}

	return nil
}

func extractPartitionsHelper(rawImageFile string, outputDir string, outputBasename string, outputSplitPartitionsFormat string) error {
	imageLoopback, err := safeloopback.NewLoopback(rawImageFile)
	if err != nil {
		return err
	}
	defer imageLoopback.Close()

	// Extract the partitions as files.
	err = extractPartitions(imageLoopback.DevicePath(), outputDir, outputBasename, outputSplitPartitionsFormat)
	if err != nil {
		return err
	}

	err = imageLoopback.CleanClose()
	if err != nil {
		return err
	}

	return nil
}

func shrinkFilesystemsHelper(buildImageFile string) error {
	imageLoopback, err := safeloopback.NewLoopback(buildImageFile)
	if err != nil {
		return err
	}
	defer imageLoopback.Close()

	// Shrink the filesystems.
	err = shrinkFilesystems(imageLoopback.DevicePath())
	if err != nil {
		return err
	}

	err = imageLoopback.CleanClose()
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

	err = shell.ExecuteLiveWithErr(1, "qemu-nbd", "-c", nbdDevice, "-f", "raw", buildImageFile)
	if err != nil {
		return fmt.Errorf("failed to connect nbd %s to image %s: %s", nbdDevice, buildImageFile, err)
	}
	defer func() {
		// Disconnect the NBD device when the function returns
		err = shell.ExecuteLiveWithErr(1, "qemu-nbd", "-d", nbdDevice)
		if err != nil {
			return
		}
	}()

	diskPartitions, err := diskutils.GetDiskPartitions(nbdDevice)
	if err != nil {
		return err
	}

	// Extract the partition block device path.
	dataPartition, err := idToPartitionBlockDevicePath(config.OS.Verity.DataPartition.IdType, config.OS.Verity.DataPartition.Id, nbdDevice, diskPartitions)
	if err != nil {
		return err
	}
	hashPartition, err := idToPartitionBlockDevicePath(config.OS.Verity.HashPartition.IdType, config.OS.Verity.HashPartition.Id, nbdDevice, diskPartitions)
	if err != nil {
		return err
	}

	// Extract root hash using regular expressions.
	verityOutput, _, err := shell.Execute("veritysetup", "format", dataPartition, hashPartition)
	if err != nil {
		return fmt.Errorf("failed to calculate root hash:\n%w", err)
	}

	var rootHash string
	rootHashRegex, err := regexp.Compile(`Root hash:\s+([0-9a-fA-F]+)`)
	if err != nil {
		// handle the error appropriately, for example:
		return fmt.Errorf("failed to compile root hash regex: %w", err)
	}

	rootHashMatches := rootHashRegex.FindStringSubmatch(verityOutput)
	if len(rootHashMatches) <= 1 {
		return fmt.Errorf("failed to parse root hash from veritysetup output")
	}
	rootHash = rootHashMatches[1]

	systemBootPartition, err := findSystemBootPartition(diskPartitions)
	if err != nil {
		return err
	}
	bootPartition, err := findBootPartitionFromEsp(systemBootPartition, diskPartitions, buildDir)
	if err != nil {
		return err
	}

	bootPartitionTmpDir := filepath.Join(buildDir, tmpParitionDirName)
	// Temporarily mount the partition.
	bootPartitionMount, err := safemount.NewMount(bootPartition.Path, bootPartitionTmpDir, bootPartition.FileSystemType, 0, "", true)
	if err != nil {
		return fmt.Errorf("failed to mount partition (%s):\n%w", bootPartition.Path, err)
	}
	defer bootPartitionMount.Close()

	grubCfgFullPath := filepath.Join(bootPartitionTmpDir, "grub2/grub.cfg")
	if err != nil {
		return fmt.Errorf("failed to stat file (%s):\n%w", grubCfgFullPath, err)
	}

	err = updateGrubConfig(config.OS.Verity.DataPartition.IdType, config.OS.Verity.DataPartition.Id,
		config.OS.Verity.HashPartition.IdType, config.OS.Verity.HashPartition.Id, rootHash, grubCfgFullPath)
	if err != nil {
		return err
	}

	err = bootPartitionMount.CleanClose()
	if err != nil {
		return err
	}

	return nil
}
