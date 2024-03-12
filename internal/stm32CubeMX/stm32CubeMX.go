/*
 * Copyright (c) 2023-2024 Arm Limited. All rights reserved.
 *
 * SPDX-License-Identifier: Apache-2.0
 */

package stm32cubemx

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/open-cmsis-pack/generator-bridge/internal/cbuild"
	"github.com/open-cmsis-pack/generator-bridge/internal/common"
	"github.com/open-cmsis-pack/generator-bridge/internal/generator"
	"github.com/open-cmsis-pack/generator-bridge/internal/utils"
	log "github.com/sirupsen/logrus"
)

var cubeEnded bool
var watcher *fsnotify.Watcher

func procWait(proc *os.Process) {
	if proc != nil {
		proc.Wait()
		log.Println("CubeMX ended")
		cubeEnded = true
		watcher.Close()
		log.Println("Watcher closed")
	}
}

func Process(cbuildYmlPath, outPath, cubeMxPath string, runCubeMx bool, pid int) error {
	var projectFile string

	cRoot := os.Getenv("CMSIS_COMPILER_ROOT")
	if len(cRoot) == 0 {
		ex, err := os.Executable()
		if err != nil {
			return err
		}
		exPath := filepath.Dir(ex)
		exPath = filepath.ToSlash(exPath)
		cRoot = path.Dir(exPath)
	}
	var generatorFile string
	err := filepath.Walk(cRoot, func(path string, f fs.FileInfo, err error) error {
		if f.Mode().IsRegular() && strings.Contains(path, "global.generator.yml") {
			generatorFile = path
			return nil
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(generatorFile) == 0 {
		return errors.New("config file 'global.generator.yml' not found")
	}

	var gParms generator.ParamsType
	err = ReadGeneratorYmlFile(generatorFile, &gParms)
	if err != nil {
		return err
	}

	var parms cbuild.ParamsType
	err = ReadCbuildYmlFile(cbuildYmlPath, outPath, &parms)
	if err != nil {
		return err
	}

	workDir := path.Dir(cbuildYmlPath)
	if parms.OutPath != "" {
		if filepath.IsAbs(parms.OutPath) {
			workDir = parms.OutPath
		} else {
			workDir = path.Join(workDir, parms.OutPath)
		}
	} else {
		workDir = path.Join(workDir, outPath)
	}
	workDir = filepath.Clean(workDir)
	workDir = filepath.ToSlash(workDir)

	err = os.MkdirAll(workDir, os.ModePerm)
	if err != nil {
		return err
	}

	cubeIocPath := workDir
	if pid >= 0 {
		lastPath := filepath.Base(cubeIocPath)
		if lastPath != "STM32CubeMX" {
			cubeIocPath = path.Join(cubeIocPath, "STM32CubeMX")
		}
		iocprojectPath := path.Join(cubeIocPath, "STM32CubeMX.ioc")
		mxprojectPath := path.Join(cubeIocPath, ".mxproject")
		for {
			log.Printf("pid of CubeMX in daemon: %d", pid)
			proc, err := os.FindProcess(pid) // this only works for windows as it is now
			if err == nil {                  // cubeMX already runs
				go procWait(proc)
				watcher, err = fsnotify.NewWatcher()
				if err != nil {
					log.Fatal(err)
				}
				//				defer watcher.Close()
				done := make(chan bool)
				// use goroutine to start the watcher
				go func() {
					var infomx0 fs.FileInfo
					changes := 0
					for {
						if changes == 0 {
							log.Println("Waiting for CubeMX \"Generate Code\"")
						}
						select {
						case event := <-watcher.Events:
							if event.Op&fsnotify.Write == fsnotify.Write {
								log.Println("Modified file:", event.Name)
								if changes == 1 {
									infomx0, _ = os.Stat(mxprojectPath)
								}
								changes++
								if changes >= 4 {
									changes = 0
									i := 1
									for ; i < 100; i++ {
										time.Sleep(time.Second)
										infomx, err := os.Stat(mxprojectPath)
										if err == nil {
											timeDiff := infomx.ModTime().Sub(infomx0.ModTime())
											seconds := timeDiff.Abs().Seconds()
											if seconds > 1 {
												break
											}
										}
									}
									if i < 100 {
										mxproject, err := IniReader(mxprojectPath, false)
										if err != nil {
											return
										}

										err = ReadContexts(iocprojectPath, parms)
										if err != nil {
											return
										}

										err = WriteCgenYml(workDir, mxproject, parms)
										if err != nil {
											return
										}
									}
								}
							}
						case err := <-watcher.Errors:
							log.Println("Errorx:", err)
							os.Exit(0)
							// return
						}
					}
				}()
				log.Printf("watching for: %s", iocprojectPath)
				if err = watcher.Add(iocprojectPath); err != nil {
					log.Println("Error:", err)
					return err
				}
				<-done
				log.Println("Watcher raus")
			}
			log.Println("Process loop")
		}
	}

	if runCubeMx {
		lastPath := filepath.Base(cubeIocPath)
		if lastPath != "STM32CubeMX" {
			cubeIocPath = path.Join(cubeIocPath, "STM32CubeMX")
		}
		cubeIocPath = path.Join(cubeIocPath, "STM32CubeMX.ioc")

		var err error
		pid := -1
		if utils.FileExists(cubeIocPath) {
			log.Printf("CubeMX with: %s", cubeIocPath)
			pid, err = Launch(cubeIocPath, "")
			if err != nil {
				return errors.New("generator '" + gParms.ID + "' missing. Install from '" + gParms.DownloadURL + "'")
			}
		} else {
			projectFile, err = WriteProjectFile(workDir, &parms)
			if err != nil {
				return nil
			}
			log.Infof("Generated file: %v", projectFile)

			pid, err = Launch("", projectFile)
			if err != nil {
				return errors.New("generator '" + gParms.ID + "' missing. Install from '" + gParms.DownloadURL + "'")
			}
		}
		log.Printf("pid of CubeMX in main: %d", pid)

		// here cubeMX runs
		cmd := exec.Command(os.Args[0])
		cmd.Args = os.Args
		cmd.Args = append(cmd.Args, "-p", fmt.Sprint(pid)) // pid of cubeMX
		if err := cmd.Start(); err != nil {                // start myself as a daemon
			log.Fatal(err)
			return err
		}
	}
	return nil
}

func Launch(iocFile, projectFile string) (int, error) {
	log.Infof("Launching STM32CubeMX...")

	const cubeEnvVar = "STM32CubeMX_PATH"
	cubeEnv := os.Getenv(cubeEnvVar)
	if cubeEnv == "" {
		return -1, errors.New("environment variable for CubeMX not set: " + cubeEnvVar)
	}

	pathJava := path.Join(cubeEnv, "jre", "bin", "java.exe")
	pathCubeMx := path.Join(cubeEnv, "STM32CubeMX.exe")

	var cmd *exec.Cmd
	if iocFile != "" {
		cmd = exec.Command(pathJava, "-jar", pathCubeMx, iocFile)
	} else if projectFile != "" {
		cmd = exec.Command(pathJava, "-jar", pathCubeMx, "-s", projectFile)
	} else {
		cmd = exec.Command(pathJava, "-jar", pathCubeMx)
	}
	if err := cmd.Start(); err != nil {
		log.Fatal(err)
		return -1, err
	}

	return cmd.Process.Pid, nil
}

func WriteProjectFile(workDir string, parms *cbuild.ParamsType) (string, error) {
	filePath := filepath.Join(workDir, "project.script")
	log.Infof("Writing CubeMX project file %v", filePath)

	var text utils.TextBuilder
	if parms.Board != "" {
		text.AddLine("loadboard", parms.Board, "allmodes")
	} else {
		text.AddLine("load", parms.Device)
	}
	text.AddLine("project name", "STM32CubeMX")
	text.AddLine("project toolchain", utils.AddQuotes("MDK-ARM V5"))

	cubeWorkDir := workDir
	if runtime.GOOS == "windows" {
		cubeWorkDir = filepath.FromSlash(cubeWorkDir)
	}
	text.AddLine("project path", utils.AddQuotes(cubeWorkDir))
	text.AddLine("SetCopyLibrary", utils.AddQuotes("copy only"))

	if utils.FileExists(filePath) {
		os.Remove(filePath)
	}

	err := os.WriteFile(filePath, []byte(text.GetLine()), 0600)
	if err != nil {
		return "", err
	}

	return filePath, nil
}

func ReadCbuildYmlFile(path, outPath string, parms *cbuild.ParamsType) error {
	log.Infof("Reading cbuild.yml file: '%v'", path)
	err := cbuild.Read(path, outPath, parms)
	if err != nil {
		return err
	}

	return nil
}

func ReadGeneratorYmlFile(path string, parms *generator.ParamsType) error {
	log.Infof("Reading generator.yml file: '%v'", path)
	err := generator.Read(path, parms)
	return err
}

var filterFiles = map[string]string{
	"system_":   "system_ file (delivered from elsewhere)",
	"Templates": "Templates file (mostly not present)",
}

func FilterFile(file string) bool {
	for key, value := range filterFiles {
		if strings.Contains(file, key) {
			log.Infof("ignoring %v: %v", value, file)
			return true
		}
	}

	return false
}

func FindMxProject(subsystem *cbuild.SubsystemType, mxprojectAll MxprojectAllType) (MxprojectType, error) {
	if len(mxprojectAll.Mxproject) == 0 {
		return MxprojectType{}, errors.New("no .mxproject read")
	} else if len(mxprojectAll.Mxproject) == 1 {
		mxproject := mxprojectAll.Mxproject[0]
		return mxproject, nil
	}

	coreName := subsystem.CoreName
	trustzone := subsystem.TrustZone
	if trustzone == "off" {
		trustzone = ""
	}
	for id := range mxprojectAll.Mxproject {
		mxproject := mxprojectAll.Mxproject[id]
		if mxproject.CoreName == coreName && mxproject.Trustzone == trustzone {
			return mxproject, nil
		}
	}

	return MxprojectType{}, nil
}

func WriteCgenYml(outPath string, mxprojectAll MxprojectAllType, inParms cbuild.ParamsType) error {
	for id := range inParms.Subsystem {
		subsystem := &inParms.Subsystem[id]

		mxproject, err := FindMxProject(subsystem, mxprojectAll)
		if err != nil {
			continue
		}
		err = WriteCgenYmlSub(outPath, mxproject, subsystem)
		if err != nil {
			return err
		}
	}

	return nil
}

func WriteCgenYmlSub(outPath string, mxproject MxprojectType, subsystem *cbuild.SubsystemType) error {
	outName := subsystem.SubsystemIdx.Project + ".cgen.yml"
	outFile := path.Join(outPath, outName)
	var cgen cbuild.CgenType

	lastPath := filepath.Base(outPath)
	var relativePathAdd string
	if lastPath != "STM32CubeMX" {
		relativePathAdd = path.Join(relativePathAdd, "STM32CubeMX")
	}
	relativePathAdd = path.Join(relativePathAdd, "MDK-ARM")

	cgen.GeneratorImport.ForBoard = subsystem.Board
	cgen.GeneratorImport.ForDevice = subsystem.Device
	cgen.GeneratorImport.Define = append(cgen.GeneratorImport.Define, mxproject.PreviousUsedKeilFiles.CDefines...)

	for _, headerPath := range mxproject.PreviousUsedKeilFiles.HeaderPath {
		headerPath, _ = utils.ConvertFilename(outPath, headerPath, relativePathAdd)
		cgen.GeneratorImport.AddPath = append(cgen.GeneratorImport.AddPath, headerPath)
	}

	cfgPath := path.Join("drv_cfg", subsystem.SubsystemIdx.Project)
	cfgPath, _ = utils.ConvertFilename(outPath, cfgPath, "")
	cgen.GeneratorImport.AddPath = append(cgen.GeneratorImport.AddPath, cfgPath)

	var groupSrc cbuild.CgenGroupsType
	var groupHalDriver cbuild.CgenGroupsType
	var groupTz cbuild.CgenGroupsType

	groupSrc.Group = "CubeMX"
	groupHalDriver.Group = "HAL Driver"
	groupHalFilter := "HAL_Driver"

	for _, file := range mxproject.PreviousUsedKeilFiles.SourceFiles {
		if FilterFile(file) {
			continue
		}
		file, _ = utils.ConvertFilename(outPath, file, relativePathAdd)

		if strings.Contains(file, groupHalFilter) {
			var cgenFile cbuild.CgenFilesType
			cgenFile.File = file
			groupHalDriver.Files = append(groupHalDriver.Files, cgenFile)
		} else {
			var cgenFile cbuild.CgenFilesType
			cgenFile.File = file
			groupSrc.Files = append(groupSrc.Files, cgenFile)
		}
	}

	cgen.GeneratorImport.Groups = append(cgen.GeneratorImport.Groups, groupSrc)
	cgen.GeneratorImport.Groups = append(cgen.GeneratorImport.Groups, groupHalDriver)

	if subsystem.TrustZone == "non-secure" {
		groupTz.Group = "CMSE Library"
		var cgenFile cbuild.CgenFilesType
		cgenFile.File = "$cmse-lib("
		cgenFile.File += subsystem.SubsystemIdx.SecureContextName
		cgenFile.File += ")$"
		groupTz.Files = append(groupTz.Files, cgenFile)
		cgen.GeneratorImport.Groups = append(cgen.GeneratorImport.Groups, groupTz)
	}

	return common.WriteYml(outFile, &cgen)
}
