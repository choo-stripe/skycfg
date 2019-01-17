// Copyright 2018 The Skycfg Authors.
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
//
// SPDX-License-Identifier: Apache-2.0

// Package skycfg is an extension library for the Starlark language that adds support
// for constructing Protocol Buffer messages.
package skycfg

import (
	"bytes"
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/golang/protobuf/proto"
	"go.starlark.net/starlark"
	"go.starlark.net/starlarkstruct"
	"go.starlark.net/starlarktest"

	impl "github.com/stripe/skycfg/internal/go/skycfg"
)

// A FileReader controls how load() calls resolve and read other modules.
type FileReader interface {
	// Resolve parses the "name" part of load("name", "symbol") to a path. This
	// is not required to correspond to a true path on the filesystem, but should
	// be "absolute" within the semantics of this FileReader.
	//
	// fromPath will be empty when loading the root module passed to Load().
	Resolve(ctx context.Context, name, fromPath string) (path string, err error)

	// ReadFile reads the content of the file at the given path, which was
	// returned from Resolve().
	ReadFile(ctx context.Context, path string) ([]byte, error)
}

type localFileReader struct {
	root string
}

// LocalFileReader returns a FileReader that resolves and loads files from
// within a given filesystem directory.
func LocalFileReader(root string) FileReader {
	if root == "" {
		panic("LocalFileReader: empty root path")
	}
	return &localFileReader{root}
}

func (r *localFileReader) Resolve(ctx context.Context, name, fromPath string) (string, error) {
	if fromPath == "" {
		return name, nil
	}
	if filepath.Separator != '/' && strings.ContainsRune(name, filepath.Separator) {
		return "", fmt.Errorf("load(%q): invalid character in module name", name)
	}
	resolved := filepath.Join(r.root, filepath.FromSlash(path.Clean("/"+name)))
	return resolved, nil
}

func (r *localFileReader) ReadFile(ctx context.Context, path string) ([]byte, error) {
	return ioutil.ReadFile(path)
}

// NewProtoMessage returns a Starlark value representing the given Protobuf
// message. It can be returned back to a proto.Message() via AsProtoMessage().
func NewProtoMessage(msg proto.Message) starlark.Value {
	return impl.NewSkyProtoMessage(msg)
}

// AsProtoMessage returns a Protobuf message underlying the given Starlark
// value, which must have been created by NewProtoMessage(). Returns
// (_, false) if the value is not a valid message.
func AsProtoMessage(v starlark.Value) (proto.Message, bool) {
	return impl.ToProtoMessage(v)
}

// A Config is a Skycfg config file that has been fully loaded and is ready
// for execution.
type Config struct {
	filename string
	globals  starlark.StringDict
	locals   starlark.StringDict
}

// A LoadOption adjusts details of how Skycfg configs are loaded.
type LoadOption interface {
	applyLoad(*loadOptions)
}

type loadOptions struct {
	globals       starlark.StringDict
	fileReader    FileReader
	protoRegistry impl.ProtoRegistry
}

type fnLoadOption func(*loadOptions)

func (fn fnLoadOption) applyLoad(opts *loadOptions) { fn(opts) }

type unstableProtoRegistry interface {
	impl.ProtoRegistry
}

// WithGlobals adds additional global symbols to the Starlark environment
// when loading a Skycfg config.
func WithGlobals(globals starlark.StringDict) LoadOption {
	return fnLoadOption(func(opts *loadOptions) {
		for key, value := range globals {
			opts.globals[key] = value
		}
	})
}

// WithAssert adds assert functionality from assert.star
// See https://github.com/google/starlark-go/blob/master/starlarktest/assert.star
func WithAssert() LoadOption {
	globals, err := starlarktest.LoadAssertModule()
	if err != nil {
		panic(fmt.Sprintf("WithAssert: unable to load assert module: %s", err))
	}
	return fnLoadOption(func(opts *loadOptions) {
		for key, value := range globals {
			opts.globals[key] = value
		}
	})
}

// WithFileReader changes the implementation of load() when loading a
// Skycfg config.
func WithFileReader(r FileReader) LoadOption {
	if r == nil {
		panic("WithFileReader: nil reader")
	}
	return fnLoadOption(func(opts *loadOptions) {
		opts.fileReader = r
	})
}

// WithProtoRegistry is an EXPERIMENTAL and UNSTABLE option to override
// how Protobuf message type names are mapped to Go types.
func WithProtoRegistry(r unstableProtoRegistry) LoadOption {
	if r == nil {
		panic("WithProtoRegistry: nil registry")
	}
	return fnLoadOption(func(opts *loadOptions) {
		opts.protoRegistry = r
	})
}

// Load reads a Skycfg config file from the filesystem.
func Load(ctx context.Context, filename string, opts ...LoadOption) (*Config, error) {
	protoModule := impl.NewProtoModule(nil /* TODO: registry from options */)
	parsedOpts := &loadOptions{
		globals: starlark.StringDict{
			"fail":   starlark.NewBuiltin("fail", skyFail),
			"hash":   impl.HashModule(),
			"json":   impl.JsonModule(),
			"proto":  protoModule,
			"struct": starlark.NewBuiltin("struct", starlarkstruct.Make),
			"yaml":   impl.YamlModule(),
			"url":    impl.UrlModule(),
		},
		fileReader: LocalFileReader(filepath.Dir(filename)),
	}
	for _, opt := range opts {
		opt.applyLoad(parsedOpts)
	}
	protoModule.Registry = parsedOpts.protoRegistry
	configLocals, err := loadImpl(ctx, parsedOpts, filename)
	if err != nil {
		return nil, err
	}
	return &Config{
		filename: filename,
		globals:  parsedOpts.globals,
		locals:   configLocals,
	}, nil
}

func loadImpl(ctx context.Context, opts *loadOptions, filename string) (starlark.StringDict, error) {
	reader := opts.fileReader

	type cacheEntry struct {
		globals starlark.StringDict
		err     error
	}
	cache := make(map[string]*cacheEntry)

	var load func(thread *starlark.Thread, moduleName string) (starlark.StringDict, error)
	load = func(thread *starlark.Thread, moduleName string) (starlark.StringDict, error) {
		var fromPath string
		if thread.TopFrame() != nil {
			fromPath = thread.TopFrame().Position().Filename()
		}
		modulePath, err := reader.Resolve(ctx, moduleName, fromPath)
		if err != nil {
			return nil, err
		}

		e, ok := cache[modulePath]
		if e != nil {
			return e.globals, e.err
		}
		if ok {
			return nil, fmt.Errorf("cycle in load graph")
		}
		moduleSource, err := reader.ReadFile(ctx, modulePath)
		if err != nil {
			cache[modulePath] = &cacheEntry{nil, err}
			return nil, err
		}

		cache[modulePath] = nil
		globals, err := starlark.ExecFile(thread, modulePath, moduleSource, opts.globals)
		cache[modulePath] = &cacheEntry{globals, err}
		return globals, err
	}
	return load(&starlark.Thread{
		Print: skyPrint,
		Load:  load,
	}, filename)
}

// Filename returns the original filename passed to Load().
func (c *Config) Filename() string {
	return c.filename
}

// Globals returns the set of variables in the Starlark global namespace,
// including any added to the config loader by WithGlobals().
func (c *Config) Globals() starlark.StringDict {
	return c.globals
}

// Locals returns the set of variables in the Starlark local namespace for
// the top-level module.
func (c *Config) Locals() starlark.StringDict {
	return c.locals
}

// An ExecOption adjusts details of how a Skycfg config's main function is
// executed.
type ExecOption interface {
	applyExec(*execOptions)
}

type execOptions struct {
	vars *starlark.Dict
}

type fnExecOption func(*execOptions)

func (fn fnExecOption) applyExec(opts *execOptions) { fn(opts) }

// WithVars adds key:value pairs to the ctx.vars dict passed to main().
func WithVars(vars starlark.StringDict) ExecOption {
	return fnExecOption(func(opts *execOptions) {
		for key, value := range vars {
			opts.vars.SetKey(starlark.String(key), value)
		}
	})
}

// Main executes main() from the top-level Skycfg config module, which is
// expected to return either None or a list of Protobuf messages.
func (c *Config) Main(ctx context.Context, opts ...ExecOption) ([]proto.Message, error) {
	parsedOpts := &execOptions{
		vars: &starlark.Dict{},
	}
	for _, opt := range opts {
		opt.applyExec(parsedOpts)
	}
	mainVal, ok := c.locals["main"]
	if !ok {
		return nil, fmt.Errorf("no `main' function found in %q", c.filename)
	}
	main, ok := mainVal.(starlark.Callable)
	if !ok {
		return nil, fmt.Errorf("`main' must be a function (got a %s)", mainVal.Type())
	}

	thread := &starlark.Thread{
		Print: skyPrint,
	}
	thread.SetLocal("context", ctx)
	mainCtx := &impl.Module{
		Name: "skycfg_ctx",
		Attrs: starlark.StringDict(map[string]starlark.Value{
			"vars": parsedOpts.vars,
		}),
	}
	args := starlark.Tuple([]starlark.Value{mainCtx})
	mainVal, err := starlark.Call(thread, main, args, nil)
	if err != nil {
		return nil, err
	}
	mainList, ok := mainVal.(*starlark.List)
	if !ok {
		if _, isNone := mainVal.(starlark.NoneType); isNone {
			return nil, nil
		}
		return nil, fmt.Errorf("`main' didn't return a list (got a %s)", mainVal.Type())
	}
	var msgs []proto.Message
	for ii := 0; ii < mainList.Len(); ii++ {
		maybeMsg := mainList.Index(ii)
		msg, ok := AsProtoMessage(maybeMsg)
		if !ok {
			return nil, fmt.Errorf("`main' returned something that's not a protobuf (a %s)", maybeMsg.Type())
		}
		msgs = append(msgs, msg)
	}
	return msgs, nil
}

// RunTests executes all functions with names of the form "test_...". Tests are expected to
// load "assert.star" and use assertions. The return values of the test invocations are discarded.
// It returns the failure messages if there were any failures, and an error if any of
// the tests failed or did not execute properly.
func (c *Config) RunTests(ctx context.Context, t *testing.T, opts ...ExecOption) ([]string, error) {
	parsedOpts := &execOptions{
		vars: &starlark.Dict{},
	}
	for _, opt := range opts {
		opt.applyExec(parsedOpts)
	}

	msgs := []string{}
	success := true
	for name, val := range c.locals {
		if !strings.HasPrefix(name, "test_") || val.Type() != "function" {
			continue
		}

		callable := val.(starlark.Callable)
		thread := &starlark.Thread{
			Print: skyPrint,
		}
		starlarktest.SetReporter(thread, t)

		thread.SetLocal("context", ctx)
		funcCtx := &impl.Module{
			Name: "skycfg_ctx",
			Attrs: starlark.StringDict(map[string]starlark.Value{
				"vars": parsedOpts.vars,
			}),
		}
		args := starlark.Tuple([]starlark.Value{funcCtx})
		_, err := starlark.Call(thread, callable, args, nil)
		if err != nil {
			msgs = append(msgs, fmt.Sprintf("Test did not exit cleanly (%s): %s", name, err))
			success = false
		}
	}

	if !success {
		return msgs, fmt.Errorf("%d tests failed to exit cleanly", len(msgs))
	}

	return nil, nil
}

func skyPrint(t *starlark.Thread, msg string) {
	fmt.Fprintf(os.Stderr, "[%v] %s\n", t.Caller().Position(), msg)
}

func skyFail(t *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var msg string
	if err := starlark.UnpackPositionalArgs(fn.Name(), args, kwargs, 1, &msg); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	t.Caller().WriteBacktrace(&buf)
	return nil, fmt.Errorf("[%s] %s\n%s", t.Caller().Position(), msg, buf.String())
}
