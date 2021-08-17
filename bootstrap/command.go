// Copyright 2014 Google Inc. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package bootstrap

import (
	"bufio"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"runtime/pprof"
	"runtime/trace"

	"github.com/google/blueprint"
)

type Args struct {
	OutFile                  string
	Subninjas                []string
	GlobFile                 string
	GlobListDir              string
	DepFile                  string
	DocFile                  string
	Cpuprofile               string
	Memprofile               string
	DelveListen              string
	DelvePath                string
	TraceFile                string
	RunGoTests               bool
	UseValidations           bool
	NoGC                     bool
	EmptyNinjaFile           bool
	BuildDir                 string
	ModuleListFile           string
	NinjaBuildDir            string
	TopFile                  string
	GeneratingPrimaryBuilder bool

	PrimaryBuilderInvocations []PrimaryBuilderInvocation
}

func PrimaryBuilderExtraFlags(args Args, mainNinjaFile string) []string {
	result := make([]string, 0)

	if args.RunGoTests {
		result = append(result, "-t")
	}

	result = append(result, "-l", args.ModuleListFile)
	result = append(result, "-o", mainNinjaFile)

	if args.EmptyNinjaFile {
		result = append(result, "--empty-ninja-file")
	}

	if args.DelveListen != "" {
		result = append(result, "--delve_listen", args.DelveListen)
	}

	if args.DelvePath != "" {
		result = append(result, "--delve_path", args.DelvePath)
	}

	return result
}

// Returns the list of dependencies the emitted Ninja files has. These can be
// written to the .d file for the output so that it is correctly rebuilt when
// needed in case Blueprint is itself invoked from Ninja
func RunBlueprint(args Args, ctx *blueprint.Context, config interface{}) []string {
	runtime.GOMAXPROCS(runtime.NumCPU())

	if args.NoGC {
		debug.SetGCPercent(-1)
	}

	if args.Cpuprofile != "" {
		f, err := os.Create(joinPath(ctx.SrcDir(), args.Cpuprofile))
		if err != nil {
			fatalf("error opening cpuprofile: %s", err)
		}
		pprof.StartCPUProfile(f)
		defer f.Close()
		defer pprof.StopCPUProfile()
	}

	if args.TraceFile != "" {
		f, err := os.Create(joinPath(ctx.SrcDir(), args.TraceFile))
		if err != nil {
			fatalf("error opening trace: %s", err)
		}
		trace.Start(f)
		defer f.Close()
		defer trace.Stop()
	}

	srcDir := filepath.Dir(args.TopFile)

	ninjaDeps := make([]string, 0)

	if args.ModuleListFile != "" {
		ctx.SetModuleListFile(args.ModuleListFile)
		ninjaDeps = append(ninjaDeps, args.ModuleListFile)
	} else {
		fatalf("-l <moduleListFile> is required and must be nonempty")
	}
	filesToParse, err := ctx.ListModulePaths(srcDir)
	if err != nil {
		fatalf("could not enumerate files: %v\n", err.Error())
	}

	buildDir := config.(BootstrapConfig).BuildDir()

	stage := StageMain
	if args.GeneratingPrimaryBuilder {
		stage = StagePrimary
	}

	mainNinjaFile := filepath.Join("$buildDir", "build.ninja")

	var invocations []PrimaryBuilderInvocation

	if args.PrimaryBuilderInvocations != nil {
		invocations = args.PrimaryBuilderInvocations
	} else {
		primaryBuilderArgs := PrimaryBuilderExtraFlags(args, mainNinjaFile)
		primaryBuilderArgs = append(primaryBuilderArgs, args.TopFile)

		invocations = []PrimaryBuilderInvocation{{
			Inputs:  []string{args.TopFile},
			Outputs: []string{mainNinjaFile},
			Args:    primaryBuilderArgs,
		}}
	}

	bootstrapConfig := &Config{
		stage: stage,

		topLevelBlueprintsFile:    args.TopFile,
		subninjas:                 args.Subninjas,
		globListDir:               args.GlobListDir,
		runGoTests:                args.RunGoTests,
		useValidations:            args.UseValidations,
		primaryBuilderInvocations: invocations,
	}

	ctx.RegisterBottomUpMutator("bootstrap_plugin_deps", pluginDeps)
	ctx.RegisterModuleType("bootstrap_go_package", newGoPackageModuleFactory(bootstrapConfig))
	ctx.RegisterModuleType("bootstrap_go_binary", newGoBinaryModuleFactory(bootstrapConfig, false))
	ctx.RegisterModuleType("blueprint_go_binary", newGoBinaryModuleFactory(bootstrapConfig, true))
	ctx.RegisterSingletonType("bootstrap", newSingletonFactory(bootstrapConfig))

	ctx.RegisterSingletonType("glob", globSingletonFactory(bootstrapConfig.globListDir, ctx))

	blueprintFiles, errs := ctx.ParseFileList(filepath.Dir(args.TopFile), filesToParse, config)
	if len(errs) > 0 {
		fatalErrors(errs)
	}

	// Add extra ninja file dependencies
	ninjaDeps = append(ninjaDeps, blueprintFiles...)

	extraDeps, errs := ctx.ResolveDependencies(config)
	if len(errs) > 0 {
		fatalErrors(errs)
	}
	ninjaDeps = append(ninjaDeps, extraDeps...)

	if c, ok := config.(ConfigStopBefore); ok {
		if c.StopBefore() == StopBeforePrepareBuildActions {
			return ninjaDeps
		}
	}

	extraDeps, errs = ctx.PrepareBuildActions(config)
	if len(errs) > 0 {
		fatalErrors(errs)
	}
	ninjaDeps = append(ninjaDeps, extraDeps...)

	if c, ok := config.(ConfigStopBefore); ok {
		if c.StopBefore() == StopBeforeWriteNinja {
			return ninjaDeps
		}
	}

	const outFilePermissions = 0666
	var out io.StringWriter
	var f *os.File
	var buf *bufio.Writer

	if args.EmptyNinjaFile {
		if err := ioutil.WriteFile(joinPath(ctx.SrcDir(), args.OutFile), []byte(nil), outFilePermissions); err != nil {
			fatalf("error writing empty Ninja file: %s", err)
		}
	}

	if stage != StageMain || !args.EmptyNinjaFile {
		f, err = os.OpenFile(joinPath(ctx.SrcDir(), args.OutFile), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, outFilePermissions)
		if err != nil {
			fatalf("error opening Ninja file: %s", err)
		}
		buf = bufio.NewWriterSize(f, 16*1024*1024)
		out = buf
	} else {
		out = ioutil.Discard.(io.StringWriter)
	}

	if args.GlobFile != "" {
		WriteBuildGlobsNinjaFile(args.GlobListDir, ctx, args, config)
	}

	err = ctx.WriteBuildFile(out)
	if err != nil {
		fatalf("error writing Ninja file contents: %s", err)
	}

	if buf != nil {
		err = buf.Flush()
		if err != nil {
			fatalf("error flushing Ninja file contents: %s", err)
		}
	}

	if f != nil {
		err = f.Close()
		if err != nil {
			fatalf("error closing Ninja file: %s", err)
		}
	}

	if c, ok := config.(ConfigRemoveAbandonedFilesUnder); ok {
		under, except := c.RemoveAbandonedFilesUnder(buildDir)
		err := removeAbandonedFilesUnder(ctx, srcDir, buildDir, under, except)
		if err != nil {
			fatalf("error removing abandoned files: %s", err)
		}
	}

	if args.Memprofile != "" {
		f, err := os.Create(joinPath(ctx.SrcDir(), args.Memprofile))
		if err != nil {
			fatalf("error opening memprofile: %s", err)
		}
		defer f.Close()
		pprof.WriteHeapProfile(f)
	}

	return ninjaDeps
}

func fatalf(format string, args ...interface{}) {
	fmt.Printf(format, args...)
	fmt.Print("\n")
	os.Exit(1)
}

func fatalErrors(errs []error) {
	red := "\x1b[31m"
	unred := "\x1b[0m"

	for _, err := range errs {
		switch err := err.(type) {
		case *blueprint.BlueprintError,
			*blueprint.ModuleError,
			*blueprint.PropertyError:
			fmt.Printf("%serror:%s %s\n", red, unred, err.Error())
		default:
			fmt.Printf("%sinternal error:%s %s\n", red, unred, err)
		}
	}
	os.Exit(1)
}

func joinPath(base, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(base, path)
}
