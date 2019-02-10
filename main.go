// Copyright 2017 Seamia Corporation. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"github.com/emicklei/proto"
	"github.com/seamia/protodot/plus"
	"github.com/seamia/tools/assets"
	"github.com/seamia/tools/support"
	"io"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"
)

type Kind int

const (
	Unknown Kind = 0
	Simple  Kind = 1 + iota
	Enum
	Message
	Missing
)

const (
	typenameRPC     = "rpc"
	typenameService = "service"
	typenameEnum    = "enum"
	typenameMessage = "message"
	typenameMissing = "missing"

	appVersion     = "generated by github.com/seamia/protodot"
	entryGenerated = "generated"
	generateSvg    = "generate .svg file"
	generatePng    = "generate .png file"
)

// use explicit string type to alleviate potential mismatch problems
type OriginalName string // name as it appears in the source
type FullName string     // fully qualified name = enough to identify the target (within given set of the source files). may have '.' in it
type UniqueName string   // a short alias for a FullName

type tinfo struct {
	fullname FullName   // "one.two.three.WhatEver"
	unique   UniqueName // "WhatEver$1"
	name     string     // "WhatEver"

	typename string // "enum", ...
	filename string
	comment  string
	raw      string

	protopack string
	parent    FullName // full type of the parent
	object    interface{}
}

type pkgInfo struct {
	packageName  string
	fileName     string
	dependencies []string
	weak         bool
	missing      bool
	proto3       bool
}

type pbstate struct {
	knownFiles  map[string]*pkgInfo
	types237    map[FullName]tinfo
	translate   map[OriginalName][]FullName
	inclusions  map[UniqueName]map[UniqueName]int
	resolutions map[FullName]map[OriginalName]FullName // maps full.name + short.type to full.type
	diveDepth   int
	counter     int
	knownNames  map[UniqueName]FullName // maps 'unique' to 'full'
	dive        bool
	proto       string
	pkg         string
	rootDir     string
	writer      *ForkWriter
	outputFile  string
	selection   string
	incMapping  map[string]string
}

func (pbs *pbstate) full2info(name FullName) *tinfo {
	if back, found := pbs.types237[name]; found {
		return &back
	}
	return nil
}

func (pbs *pbstate) unique2info(name UniqueName) *tinfo {
	if full, found := pbs.knownNames[name]; found {
		return pbs.full2info(full)
	}
	return nil
}

func (pbs *pbstate) currentPkgInfo() *pkgInfo {
	if pbs != nil && pbs.knownFiles != nil && len(pbs.proto) > 0 {
		if info, found := pbs.knownFiles[pbs.proto]; found {
			return info
		}
	}
	assert("somehow there is no pkgInfo available...")
	return &pkgInfo{}
}

func NewPbs() *pbstate {
	one := pbstate{}
	one.knownFiles = make(map[string]*pkgInfo)
	one.types237 = make(map[FullName]tinfo)
	one.translate = make(map[OriginalName][]FullName)
	one.inclusions = make(map[UniqueName]map[UniqueName]int)
	one.resolutions = make(map[FullName]map[OriginalName]FullName)

	one.counter = 100
	one.knownNames = make(map[UniqueName]FullName)

	one.dive = true

	one.writer = NewForkWriter()

	return &one
}

func (pbs *pbstate) AddWriter(target io.Writer) {
	pbs.writer.AddWriter(target)
}

func (pbs *pbstate) target() io.Writer {
	if pbs.writer != nil {
		return pbs.writer
	} else {
		return os.Stdout
	}
}

func (pbs *pbstate) addIncMapping(mapping map[string]string) {
	if mapping != nil && len(mapping) > 0 {
		if pbs.incMapping == nil {
			pbs.incMapping = make(map[string]string)
		}
		for k, v := range mapping {
			pbs.incMapping[k] = v
		}
	}
}

func (pbs *pbstate) getUniqueName(short OriginalName, full FullName) UniqueName {

	if got, found := pbs.knownNames[UniqueName(short)]; found && got == full {
		return UniqueName(short)
	}

	name := UniqueName(fmt.Sprintf("Ja_%d", pbs.counter))
	pbs.counter++
	pbs.knownNames[name] = full
	return UniqueName(name)
}

func (pbs *pbstate) addResolution(scope FullName, shorttype OriginalName, fulltype FullName) {

	if _, found := pbs.resolutions[scope]; !found {
		pbs.resolutions[scope] = make(map[OriginalName]FullName)
	}

	pbs.resolutions[scope][shorttype] = fulltype
}

func (pbs *pbstate) getResolution(scope FullName, shorttype OriginalName) *tinfo {

	if fulltype, found := pbs.resolutions[scope][shorttype]; found {
		if info, found := pbs.types237[fulltype]; found {
			return &info
		}
	}

	for _, info := range pbs.types237 {
		if info.typename == typenameMissing && info.name == string(shorttype) {
			return &info
		}
	}

	// alert("*** failed to resolve type:", shorttype) - it is okay to fail resolutuin (while we're resolving)
	return nil
}

func (pbs *pbstate) recordInclusion(from UniqueName, field string, to UniqueName) {

	fullFrom := from
	if len(field) > 0 {
		fullFrom += UniqueName(":" + field)
	}

	if _, there := pbs.inclusions[fullFrom]; !there {
		pbs.inclusions[fullFrom] = make(map[UniqueName]int)
	}
	pbs.inclusions[fullFrom][to]++
}

func renderMissingNode(name OriginalName, unique UniqueName, fullname FullName) string {

	writer := bytes.NewBufferString("")
	payload := EnumPayload{
		Name:     string(name),
		Unique:   unique,
		FullName: fullname,
	}
	if err := plus.ApplyTemplate("missing.node", writer, payload); err != nil {
		alert("failed to render", err)
		return ""
	}

	return writer.String()
}

func (pbs *pbstate) recordMissingType(from UniqueName, missingType OriginalName) UniqueName {

	fulltype := FullName("missing." + string(from) + "." + string(missingType))
	if info, found := pbs.types237[fulltype]; !found {
		unique := pbs.getUniqueName(missingType, fulltype)
		pbs.types237[fulltype] = tinfo{
			typename:  typenameMissing,
			fullname:  fulltype,
			unique:    unique,
			name:      string(missingType),
			protopack: pbs.proto,
			raw:       renderMissingNode(OriginalName(missingType), unique, fulltype),
		}
		return unique
	} else {
		return info.unique
	}
}

func (pbs *pbstate) recordMissingInclusion(from UniqueName, field string, missingType OriginalName) {
	debug("****** Field [", field, "] from [", from, "] refers to non-existing type [", missingType, "] ******")

	if options("show missing types") {
		// 1. save type (if not already)
		unique := pbs.recordMissingType(from, missingType)

		// 2. record the connection
		pbs.recordInclusion(from, field, unique)
	}
}

func (pbs *pbstate) getInclusion(from UniqueName, field string) (UniqueName, map[UniqueName]int) {

	fullFrom := from
	if len(field) > 0 {
		fullFrom += UniqueName(":" + field)
	}
	if inc, found := pbs.inclusions[fullFrom]; found {
		return fullFrom, inc
	}
	return "", nil
}

func (pbs *pbstate) applyTemplate(name string, payload interface{}) {
	if err := plus.ApplyTemplate(name, pbs.target(), payload); err != nil {
		alert("failed to render", err)
	}
}

func (pbs *pbstate) expandSelection(selection string) ([]FullName, error) {
	matches := make([]FullName, 0)

	// deal with the special case(s) first
	if selection == "*" {
		// include only entities defined in the root file (and their dependencies)
		for fulltype, info := range pbs.types237 {
			if info.protopack == pbs.proto {
				matches = append(matches, fulltype)
			} else {
				debug("            excluding:", fulltype)
			}
		}
		return matches, nil
	}
	for _, root := range strings.Split(selection, ";") {
		if len(root) == 0 {
			continue
		}
		locals := make([]FullName, 0)
		for fulltype, _ := range pbs.types237 {
			if strings.HasSuffix(string(fulltype), root) {
				locals = append(locals, fulltype)
			}
		}

		if len(locals) == 0 {
			// let's do a more relaxed search
			for fulltype, _ := range pbs.types237 {
				if strings.Index(string(fulltype), root) >= 0 {
					locals = append(locals, fulltype)
				}
			}
		}

		if len(locals) == 0 {
			status("Cannot find anything matching your selection:", root)
			return nil, errors.New("Cannot find anything matching your selection:" + root)
		}
		if len(locals) > 1 {
			trace("Your selection ["+root+"] results in more than one entry:", locals)
			return nil, errors.New(fmt.Sprint("Your selection ["+root+"] results in more than one entry:", locals))
		}
		matches = append(matches, locals[0])
	}
	return matches, nil
}

func (pbs *pbstate) showSelectedInclusion(selection string) {
	// pbs.types237
	// pbs.inclusions
	status("limiting output to the following: ", selection)
	matches, err := pbs.expandSelection(selection)
	if err != nil {
		status(err.Error())
		return
	}

	// create new storage for the selections and their dependants
	types := make(map[FullName]tinfo)
	posttypes := make(map[FullName]tinfo)
	inclusions := make(map[UniqueName]map[UniqueName]int)

	for index, _ := range matches {
		info := pbs.types237[matches[index]]

		if len(info.parent) > 0 {
			parentInfo := pbs.types237[info.parent] // types237   map[FullName]tinfo
			debug("", parentInfo)
		}

		debug("type of the selection:", info.typename)
		switch info.typename {
		case typenameService:
			// just works =)
			debug("------", info)
		case typenameRPC:
			// this is a bit elaborate
			// service
			//   ...
			//   rpc -> request, response
			//   ...
			rpc, ok := info.object.(*proto.RPC)
			if ok {
				parentType := info.parent
				requestType := rpc.RequestType
				returnsType := rpc.ReturnsType

				// add 'parent' directly without all of its children
				posttypes[parentType] = pbs.types237[parentType]

				// add connections from 'parent' too children
				for _, suffix := range []string{"_request", "_response"} {
					from, to := pbs.getInclusion(pbs.types237[parentType].unique, rpc.Name+suffix)
					if to != nil {
						inclusions[from] = to
					}
				}

				for _, one := range []string{requestType, returnsType} {
					if inf := pbs.getResolution(parentType, OriginalName(one)); inf != nil {
						matches = append(matches, inf.fullname)
					} else {
						// todo: react here? maybe?
					}
				}
			} else {
				assert("failed to get an expected type")
			}

		case typenameMessage:
			// nothing special here to do
			debug("------", info)
		default:
			alert("entry of type [", info.typename, "] is not yet supported.")
		}
	}

	for len(matches) > 0 {
		candidate := matches[0]
		matches = matches[1:]
		trace("---------------------------- considering: ", candidate)

		if _, found := types[candidate]; found {
			trace("          already added:", candidate)
			continue
		}
		types[candidate] = pbs.types237[candidate]
		unique := types[candidate].unique + ":"

		for key, value := range pbs.inclusions {
			if strings.HasPrefix(string(key), string(unique)) {
				trace("          checking [", key, "]")
				for child, _ := range value {
					if fullchild, found := pbs.knownNames[child]; found {
						if _, found := types[fullchild]; !found {
							// we have not seen this type before
							matches = append(matches, fullchild)
							trace("              adding [", child, "] [", value, "]")
						} else {
							trace("              already included [", fullchild, "]")
						}
						inclusions[key] = value
					} else {
						trace("              failed to find [", child, "]")
					}
				}
			} else {
				trace("          excluding [", key, "] cause it has no prefix [", unique, "]")
			}
		}
	}

	// copy posttypes to types237
	for k, v := range posttypes {
		types[k] = v
	}

	{
		tmp := make([]string, 0, len(types))
		for _, info := range types {
			tmp = append(tmp, info.name)
		}
		trace("for your selections found the following dependencies:", tmp)
	}

	backupTypes, backupInclusions := pbs.types237, pbs.inclusions
	pbs.types237, pbs.inclusions = types, inclusions
	pbs.showInclusion(false, true)
	pbs.types237, pbs.inclusions = backupTypes, backupInclusions
}

func (pbs *pbstate) showInclusion(groupByPackages bool, leaveRootPackageUnwrapped bool) {

	payload := PBS{
		Package:    pbs.pkg,
		Protoname:  pbs.proto,
		AppVersion: appVersion,
		Timestamp:  time.Now().Format(time.RFC850),
		Selection:  pbs.selection,
		Options:    "",
	}

	pbs.applyTemplate("document.header", payload)
	pbs.applyTemplate("comment", "nodes")

	if groupByPackages {
		groups := make(map[string][]tinfo)
		for _, info := range pbs.types237 {
			if _, present := groups[info.protopack]; !present {
				groups[info.protopack] = make([]tinfo, 0)
			}
			groups[info.protopack] = append(groups[info.protopack], info)
		}

		for group, members := range groups {
			components := strings.Split(group, string(os.PathSeparator))

			data := Cluster{
				ProtoName:       strings.Replace(group, "\\", "\\\\", -1),
				ProtoNameKosher: support.NameToId(group, 12),
				ShortName:       components[len(components)-1],
			}

			if leaveRootPackageUnwrapped && group == pbs.proto {

				pbs.applyTemplate("comment", "leaving the root package unwrapped")
				for _, info := range members {
					pbs.applyTemplate("entry", info.raw)
				}
			} else {

				pbs.applyTemplate("cluster.prefix", data)
				for _, info := range members {
					pbs.applyTemplate("cluster.entry", info.raw)
				}
				pbs.applyTemplate("cluster.suffix", data)
			}
		}
	} else {
		for _, info := range pbs.types237 {
			pbs.applyTemplate("entry", info.raw)
		}
	}

	pbs.applyTemplate("comment", "connections")

	var toTemplateName = map[string]string{
		typenameEnum:    "from.to.enum",
		typenameMessage: "from.to.message",
		typenameMissing: "from.to.missing",
	}

	// from, field, to
	for from, tos := range pbs.inclusions {
		for to, _ := range tos {

			bits := strings.Split(string(from), ":")
			args := Relationship{
				From:   bits[0],
				To:     to, // UniqueName
				ToName: "", // todo: fill these up later
				ToType: "", // FullName
			}

			if len(bits) > 1 {
				args.Field = bits[1]
			}

			tmplName := toTemplateName[pbs.types237[pbs.knownNames[to]].typename]
			pbs.applyTemplate(tmplName, args)
			// pbs.applyTemplate(isMessage[pbs.uniqueIsMessage(to)], args)
		}
	}

	pbs.applyTemplate("document.footer", payload)
}

func (pbs *pbstate) uniqueIsMessage(unique UniqueName) bool {
	if full, found := pbs.knownNames[unique]; found {
		if info, found := pbs.types237[full]; found {
			if info.typename == typenameMessage {
				return true
			}
		}
	}
	return false
}

func (pbs *pbstate) handleSyntax(syntax *proto.Syntax) {
	trace("\tsyntax:", syntax.Value)

	pbs.currentPkgInfo().proto3 = (syntax.Value == "proto3")
}

func (pbs *pbstate) handleImport(imp *proto.Import) {
	trace("\timport:", imp.Filename)

	if pbs.dive {
		prev, prev_pkg := pbs.proto, pbs.pkg
		self := pbs.currentPkgInfo()
		self.dependencies = append(self.dependencies, imp.Filename)

		pbs.diveDepth++
		debug("-- leaving [", pbs.proto, "] and diving into", imp.Filename)
		if !process(pbs, imp.Filename, "") {
			// this file was missing ...
		}
		pbs.diveDepth--
		pbs.pkg, pbs.proto = prev_pkg, prev
		// pbs.proto = prev
		debug("-- back to [", pbs.proto, "]")
	}
}

func (pbs *pbstate) handlePackageDeclaration(pkg *proto.Package) {
	trace("\tpackage:", pkg.Name)
	pbs.pkg = pkg.Name

	pbs.currentPkgInfo().packageName = pkg.Name
}

func (pbs *pbstate) saveMapping(short OriginalName, full FullName) {

	// let's try to detect collisions
	if _, found := pbs.translate[short]; found {
		debug("ERROR: there is a collision for name:", short)
	} else {
		pbs.translate[short] = make([]FullName, 0, 1)
	}

	pbs.translate[short] = append(pbs.translate[short], full) // todo: do you need 'translate' ?
}

func (pbs *pbstate) handleEnumDeclaration(e *proto.Enum) {

	fullname := getFullName(e)
	unique := pbs.getUniqueName(OriginalName(e.Name), fullname)

	pbs.saveMapping(OriginalName(e.Name), fullname)

	writer := bytes.NewBufferString("")

	payload := EnumPayload{
		Name:     e.Name,
		Unique:   unique,
		FullName: fullname,
	}
	if err := plus.ApplyTemplate("enum.prefix", writer, payload); err != nil {
		alert("failed to render", err)
	}

	for _, element := range e.Elements {
		switch actual := element.(type) {
		case *proto.EnumField:
			payload.Name = actual.Name
			payload.Value = strconv.Itoa(actual.Integer)
			if err := plus.ApplyTemplate("enum.entry", writer, payload); err != nil {
				alert("failed to render", err)
			}
		case *proto.Option:
			ignoring("ignoring options for now")
		case *proto.Comment:
			ignoring("ignoring comment for now")
		case *proto.Reserved:
			ignoring("ignoring Reserved for now")
		default:
			rname := reflect.TypeOf(actual).Elem().Name()
			unhandled("\t", "UNKNOWN2", actual, "", rname)
		}
	}

	payload.Value = ""
	if err := plus.ApplyTemplate("enum.suffix", writer, payload); err != nil {
		alert("failed to render", err)
	}

	pbs.types237[fullname] = tinfo{
		typename:  typenameEnum,
		fullname:  fullname,
		unique:    unique,
		name:      e.Name,
		filename:  e.Position.Filename,
		raw:       writer.String(),
		protopack: pbs.pkg,
	}
}

func (pbs *pbstate) dbgPrintKnownResolutions(fullname FullName) {
	debug("-------------------------------------- all known resolutions for:", fullname)
	if all, found := pbs.resolutions[fullname]; found { // map[FullName]map[OriginalName]FullName
		for k, v := range all {
			debug("      ", k, " ---> ", v)
		}
	}
	debug("--------------------------------------")
}

var typename2kind = map[string]Kind{
	typenameEnum:    Enum,
	typenameMessage: Message,
	typenameMissing: Missing,
}

func (pbs *pbstate) getKind(fullname FullName, what OriginalName) Kind {

	if isSimpleType(string(what)) {
		return Simple
	}

	if info := pbs.getResolution(fullname, what); info != nil {
		if kind, found := typename2kind[info.typename]; found {
			return kind
		}

		assert("Unknown typename [", info.typename, "] find while resolving type: ", what)
		return Unknown
	}

	pbs.dbgPrintKnownResolutions(fullname)

	assert("Unresolved type: ", what, "; source: ", fullname)
	return Unknown
}

func getPackageName(pro *proto.Proto) string {

	for _, element := range pro.Elements {
		switch actual := element.(type) {
		case *proto.Package:
			return actual.Name
		}
	}
	alert("Failed to find package name for: " + pro.Filename)
	return ""
}

const separator string = "."

func getParent(what proto.Visitee) string {
	cmd := ""
	switch parent := what.(type) {
	case *proto.Proto:
		cmd = getPackageName(parent)

	case *proto.Message:
		cmd = getParent(parent.Parent) + separator + parent.Name // the message declared in another message scope

	case *proto.Group:
		ignoring("ignoring group for now")

	default:
		rname := reflect.TypeOf(parent).Elem().Name()
		unhandled("\t", "UNKNOWN3", parent, "", rname)
	}
	return cmd
}

func getFullName(what interface{}) FullName {
	switch actual := what.(type) {
	case *proto.Message:
		return FullName(getParent(actual.Parent) + separator + actual.Name)
	case *proto.Enum:
		return FullName(getParent(actual.Parent) + separator + actual.Name)
	case *proto.Service:
		return FullName(getParent(actual.Parent) + separator + actual.Name)
	default:
		panic("not yet supported type")
	}
}

func (pbs *pbstate) handleMessageDeclaration(msg *proto.Message) {

	if msg.IsExtend {
		debug("-- excluding 'extend' messages:", msg.Name)
		return
	}

	parent := getParent(msg.Parent)
	fullname := getFullName(msg)
	unique := pbs.getUniqueName(OriginalName(msg.Name), fullname)

	pbs.saveMapping(OriginalName(msg.Name), fullname)

	debug("*** type definition:", pbs.pkg, ">>", msg.Name, ">>", parent, ">>>>>>>>", fullname)

	pbs.types237[fullname] = tinfo{
		typename: typenameMessage,
		fullname: fullname,
		unique:   unique,
		name:     msg.Name,

		filename:  msg.Position.Filename,
		comment:   parent,
		protopack: pbs.proto,
	}
}

func (pbs *pbstate) resolveType(full FullName, local OriginalName) {

	if isSimpleType(string(local)) {
		// no need to resolve simple types237
		return
	}

	if info := pbs.getResolution(full, local); info != nil {
		// looks like we already know what 'local' type maps to
		return
	}

	if occurences := len(pbs.translate[local]); occurences > 1 {

		var found FullName
		for _, one := range pbs.translate[local] {
			namespace := one[:len(one)-len(local)-len(separator)]
			if strings.HasPrefix(string(full), string(namespace)) {
				if len(one) > len(found) {
					found = one
				}
			}
		}

		if len(found) > 0 {
			trace("", full, ", mapping ", local, " to ", found)
			pbs.addResolution(full, local, found)
		} else {
			alert("!! there is more than one definition of type [", local, "], used in ", full, "", pbs.translate[local])
		}

	} else {
		parts := strings.Split(string(local), ".")
		if len(parts) > 1 {
			var found FullName
			for typename, typeinfo := range pbs.types237 {
				if strings.HasSuffix(string(typename), string(local)) {
					prefix := typename[:len(typename)-len(local)]
					if strings.HasSuffix(string(prefix), separator) {
						prefix = prefix[:len(prefix)-len(separator)]
					}

					if strings.HasPrefix(string(full), string(prefix)) {
						if len(typename) > len(found) {
							found = typename
						}
					}
					_ = typeinfo
					trace("actual:", local, "; prefix:", prefix, "; full type", typename)
				}
			}

			if len(found) > 0 {
				trace("", full, ", mapping ", local, " to ", found)
				pbs.addResolution(full, local, found)
			} else {
				alert("!! failed to find full.type.name for type [", local, "], used in ", full)
			}
		} else {
			if names, found := pbs.translate[local]; found && len(names) == 1 {
				pbs.addResolution(full, local, names[0])
			} else {
				alert("failed to resolve type:", local, "; scope:", full)
			}
		}
	}
}

func (pbs *pbstate) handleMessageTypeResolution(msg *proto.Message) {

	fullname := getFullName(msg)

	for _, element := range msg.Elements {
		switch actual := element.(type) {
		case *proto.Oneof:
			for _, element := range actual.Elements {
				switch fact := element.(type) {
				case *proto.OneOfField:
					if !isSimpleType(fact.Type) {
						pbs.resolveType(fullname, OriginalName(fact.Type))
					}
				}
			}

		case *proto.NormalField:
			pbs.resolveType(fullname, OriginalName(actual.Type))

		case *proto.MapField:
			pbs.resolveType(fullname, OriginalName(actual.Type))
		}
	}
}

func (pbs *pbstate) handleServiceTypeResolution(srv *proto.Service) {

	fullname := getFullName(srv)
	for _, element := range srv.Elements {
		switch actual := element.(type) {
		case *proto.RPC:
			pbs.resolveType(fullname, OriginalName(actual.RequestType))
			pbs.resolveType(fullname, OriginalName(actual.ReturnsType))
		}
	}
}

var isRepeated = map[bool]string{
	false: "",
	true:  "[...]",
}

func (pbs *pbstate) handleMessageBody(msg *proto.Message) {

	if msg.IsExtend {
		debug("-- excluding 'extend' messages:", msg.Name)
		return
	}

	full := getFullName(msg)
	info := pbs.types237[full]

	message := msg.Name
	debug("message", msg.Name, "-------------------------------------")

	t := newTable(message, info.fullname, info.unique, "style")

	for _, element := range msg.Elements {
		switch actual := element.(type) {
		case *proto.NormalField:

			if !isSimpleType(actual.Type) {
				if inf := pbs.getResolution(full, OriginalName(actual.Type)); inf != nil {
					pbs.encounteredType(info.unique, actual.Name, inf.unique)
				} else {
					alert("failed to resolve", actual.Type)
					pbs.recordMissingInclusion(info.unique, actual.Name, OriginalName(actual.Type))
				}
			}

			repeated := isRepeated[actual.Repeated]
			t.addRow(repeated, actual.Type, actual.Name, strconv.Itoa(actual.Sequence), pbs.getKind(full, OriginalName(actual.Type)))
			break

		case *proto.Enum:
			debug("\t", "enum:", actual.Name)
		case *proto.Reserved:
			debug("\t", "reserved:", actual.FieldNames)
		case *proto.Option:
			debug("\t", "option:", actual.Name)
		case *proto.Message:
			debug("\t", "message:", actual.Name)
		case *proto.Oneof:
			pbs.onOneof(full, info.unique, actual)
			t.addOneof(full, actual, pbs)
		case *proto.MapField:
			debug("\t", "map-field:", actual.Name, ",   map<", actual.KeyType, ", ", actual.Type, ">")
			// Q: can map be 'repeated' ?
			t.addMapRow(actual.Name, actual.KeyType, actual.Type, strconv.Itoa(actual.Sequence), pbs.getKind(full, OriginalName(actual.Type)))

			if !isSimpleType(actual.Type) {
				if inf := pbs.getResolution(full, OriginalName(actual.Type)); inf != nil {
					pbs.recordInclusion(info.unique, actual.Name, inf.unique)
				} else {
					alert("failed to resolve type [", actual.Type, "] from ", full)
					pbs.recordMissingInclusion(info.unique, actual.Name, OriginalName(actual.Type))
				}
			}

		case *proto.Comment:
			ignoring("\t", "comment:", actual.Message())

		case *proto.Extensions:
			ignoring("\t", "extensions:", "--ignored for now")

		case *proto.Group:
			ignoring("ignoring group for now")

		default:
			rname := reflect.TypeOf(actual).Elem().Name()
			unhandled("\t", "UNKNOWN4", actual, "", rname)
		}
	}

	info.raw = t.generate()
	pbs.types237[full] = info
}

func (pbs *pbstate) onOneof(fullname FullName, unique UniqueName, one *proto.Oneof) {
	debug("oneof", one.Name)
	if len(one.Elements) > 0 {
		for _, element := range one.Elements {
			switch actual := element.(type) {
			case *proto.OneOfField:
				debug("\t", "one-of-field:", actual.Name, ", type:", actual.Type)

				if !isSimpleType(actual.Type) {
					if inf := pbs.getResolution(fullname, OriginalName(actual.Type)); inf != nil {
						pbs.encounteredType(unique, actual.Name, inf.unique)
					} else {
						alert("failed to get unique name for type", actual.Type)
						pbs.recordMissingInclusion(unique, actual.Name, OriginalName(actual.Type))
					}
				}

			case *proto.Option:
				ignoring("ignoring options for now")

			case *proto.Comment:
				ignoring("ignoring comments for now")

			case *proto.Group:
				ignoring("ignoring group for now")

			default:
				rname := reflect.TypeOf(actual).Elem().Name()
				unhandled("\t", "UNKNOWN5", actual, "", rname)
			}
		}
	}
}

func (pbs *pbstate) handleOption(opt *proto.Option) {
	value := opt.Constant.Source
	debug("\t\t", "option", opt.Name, ":", value)

	for _, one := range opt.AggregatedConstants {
		debug("\t", "\t", "constant:", one.Name, ">>>", one.Literal.Source)
	}
}

var isStreaming = map[bool]string{
	false: "",
	true:  "stream",
}

func (pbs *pbstate) handleServiceDeclaration(srv *proto.Service) {

	name := getFullName(srv)
	pbs.saveMapping(OriginalName(srv.Name), name) // todo: need this?
	srvUniqueName := pbs.getUniqueName(OriginalName(srv.Name), name)

	cmd := ""
	switch parent := srv.Parent.(type) {
	case *proto.Proto:
		cmd = parent.Filename // the message declared in the file scope
	case *proto.Message:
		cmd = parent.Name // the message declared in another message scope
	default:
		rname := reflect.TypeOf(parent).Elem().Name()
		unhandled("\t", "UNKNOWN6", parent, "", rname)
	}

	writer := bytes.NewBufferString("")
	payload := ServicePayload{
		Name:     srv.Name,
		Unique:   srvUniqueName,
		FullName: name,
	}
	if err := plus.ApplyTemplate("service.prefix", writer, payload); err != nil {
		alert("failed to render", err)
	}

	for _, element := range srv.Elements {
		switch actual := element.(type) {
		case *proto.RPC:
			fullname := name + FullName("."+actual.Name)
			pbs.saveMapping(OriginalName(actual.Name), fullname)

			pbs.types237[fullname] = tinfo{
				typename:  typenameRPC,
				fullname:  fullname,
				unique:    pbs.getUniqueName(OriginalName(actual.Name), fullname),
				name:      actual.Name,
				filename:  srv.Position.Filename,
				comment:   cmd,
				protopack: pbs.proto,
				parent:    name,
				object:    actual,
			}

			payload := RPC{
				Name:           actual.Name,
				RequestType:    actual.RequestType,
				ReturnsType:    actual.ReturnsType,
				StreamsRequest: isStreaming[actual.StreamsRequest],
				StreamsReturns: isStreaming[actual.StreamsReturns],
			}
			if err := plus.ApplyTemplate("service.rpc", writer, payload); err != nil {
				alert("failed to render", err)
			}
		default:
			// unhandled("UNKNOWN21")
		}
	}

	if err := plus.ApplyTemplate("service.suffix", writer, payload); err != nil {
		alert("failed to render", err)
	}

	pbs.types237[name] = tinfo{
		typename:  typenameService,
		fullname:  name,
		unique:    srvUniqueName,
		name:      srv.Name,
		filename:  srv.Position.Filename,
		comment:   cmd,
		protopack: pbs.proto,
		raw:       writer.String(),
		object:    srv,
	}
}

func (pbs *pbstate) handleServiceBody(srv *proto.Service) {
	trace("Service", srv.Name)

	full := getFullName(srv)
	info := pbs.full2info(full)

	for _, element := range srv.Elements {
		switch actual := element.(type) {
		case *proto.RPC:
			trace("\tMethod", actual.Name, "(", actual.RequestType, ")", actual.ReturnsType)
			payload := RPC{
				Name:           actual.Name,
				RequestType:    actual.RequestType,
				ReturnsType:    actual.ReturnsType,
				StreamsRequest: isStreaming[actual.StreamsRequest],
				StreamsReturns: isStreaming[actual.StreamsReturns],
			}
			_ = payload

			// request
			if !isSimpleType(actual.RequestType) {
				field := actual.Name + "_request"
				if inf := pbs.getResolution(full, OriginalName(actual.RequestType)); inf != nil {
					pbs.recordInclusion(info.unique, field, inf.unique)
				} else {
					alert("failed to resolve type [", actual.RequestType, "] from ", full)
					pbs.recordMissingInclusion(info.unique, field, OriginalName(actual.RequestType))
				}
			}

			// response
			if !isSimpleType(actual.ReturnsType) {
				field := actual.Name + "_response"
				if inf := pbs.getResolution(full, OriginalName(actual.ReturnsType)); inf != nil {
					pbs.recordInclusion(info.unique, field, inf.unique)
				} else {
					alert("failed to resolve type [", actual.ReturnsType, "] from ", full)
					pbs.recordMissingInclusion(info.unique, field, OriginalName(actual.ReturnsType))
				}
			}

		case *proto.Option:
			ignoring("ignoring options for now")

		case *proto.Comment:
			ignoring("ignoring comments for now")

		default:
			rname := reflect.TypeOf(actual).Elem().Name()
			unhandled("\t", "UNKNOWN7", actual, "", rname)
		}
	}
}

func (pbs *pbstate) encounteredType(parent UniqueName, field string, typ UniqueName) {
	pbs.recordInclusion(parent, field, typ)
}

var import2template = map[bool]string{
	false: "imports.node",
	true:  "imports.node.missing",
}

func (pbs *pbstate) showDependencyTree() {
	if pbs.diveDepth != 0 {
		assert("need to be at the root")
		return
	}

	getID := func(name string) string {
		return "N" + support.NameToId(name, 16)
	}

	correctRootFileName := func(name string) string {
		if name == pbs.proto {
			parts := strings.Split(strings.Replace(name, "\\", "/", -1), "/")
			return parts[len(parts)-1]
		}
		return name
	}

	payload := PBS{
		Package:    pbs.pkg,
		Protoname:  pbs.proto,
		AppVersion: appVersion,
		Timestamp:  time.Now().Format(time.RFC850),
		Selection:  "(imports dependency)",
		Options:    "",
	}

	pbs.applyTemplate("imports.header", payload)

	pbs.applyTemplate("comment", "nodes")
	for name, info := range pbs.knownFiles {
		payload := ImportNode{
			NodeName:    getID(name),
			PackageName: info.packageName,
			FileName:    correctRootFileName(info.fileName),
			Status:      "",
		}
		pbs.applyTemplate(import2template[info.missing], payload)
		_ = info
	}

	pbs.applyTemplate("comment", "connections")
	for name, info := range pbs.knownFiles {
		payload := ImportLink{
			From: getID(name),
		}
		for _, toname := range info.dependencies {
			payload.To = getID(toname)
			pbs.applyTemplate("imports.connection", payload)
		}
	}

	pbs.applyTemplate("imports.footer", payload)
}

func process(pbs *pbstate, name string, selection string) bool {

	original := name
	if len(pbs.incMapping) > 0 {
		if replace, found := pbs.incMapping[name]; found {
			trace("replacing [", name, "] with [", replace, "]")
			name = replace
		}
	}

	var reader io.Reader = nil
	// need to differenciate between url/path and actual source
	if strings.Count(name, "\n") > 1 {
		trace("this seems to be a source code blob")
		reader = strings.NewReader(name)

		if name == original {
			// it appears that we're given the blob directly
			original = "blob_" + support.Hash([]byte(name))
		}

		if pbs.diveDepth == 0 {
			pbs.rootDir = "~fake~"
			pbs.selection = selection
		}

	} else if strings.HasSuffix(strings.ToLower(name), ".proto") {
		//

	} else {
		assert("undetected type of input:", name)
	}

	if inf, found := pbs.knownFiles[original]; found {
		// we already dealt with this one
		return !inf.missing
	}

	if reader == nil {
		var err error
		if matches, err := filepath.Glob(name); err == nil {
			if len(matches) > 1 {
				for _, match := range matches {
					// process found files individually
					pbs2 := NewPbs()
					process(pbs2, match, selection)
				}

				// todo: need to output combined result here, maybe?

				return true // todo: why?
			} else {
				// fall through and just do 'single file' mode
			}
		} else {
			assert("failed to find:", name, "; err:", err)
			return false
		}

		if pbs.diveDepth == 0 {
			pbs.rootDir, _ = pathSplit(name)
			pbs.selection = selection
		}

		reader, err = Find(name, pbs.rootDir)
		if err != nil {
			if pbs.diveDepth > 0 && options("allow missing imports") {
				// failed to find/open an import, but since this is not a main file and we're allowed to continue: do so

				// remember the fact that this .proto is missing
				pbs.knownFiles[original] = &pkgInfo{
					fileName: original,
					missing:  true,
				}
			} else {
				alert("failed to open", name, ", with error:", err)
				panic("failed to open [" + name + "], with error: " + err.Error())
			}
			return false
		}
	}

	if pbs.diveDepth == 0 {

		genDir, err := support.GetLocation(g_config, entryGenerated)
		if err != nil {
			trace("missing 'generated' location in the provided config")
			genDir = ""
		}

		outputFileName := getProtoName(original, pbs.selection)
		if len(*g_output) > 0 {
			outputFileName = *g_output
		}

		target := path.Join(genDir, outputFileName+".dot")
		pbs.outputFile = target
		pbs.AddWriter(NewCreateOnWrite(target))
	}

	parser := proto.NewParser(reader)
	definition, _ := parser.Parse()
	definition.Filename = original

	trace("\tprocessing file:", definition.Filename)
	pbs.knownFiles[original] = &pkgInfo{
		fileName:     original,
		dependencies: make([]string, 0),
	}
	pbs.proto = original

	proto.Walk(definition,
		WithSyntax(pbs.handleSyntax),
		WithImport(pbs.handleImport),
		proto.WithEnum(pbs.handleEnumDeclaration),
		proto.WithMessage(pbs.handleMessageDeclaration),
		WithPackage(pbs.handlePackageDeclaration),
		proto.WithOption(pbs.handleOption),
		proto.WithService(pbs.handleServiceDeclaration),
	)

	debug("------------ all known types237:")
	for key, value := range pbs.types237 {
		debug("---", key, "---", value)
	}
	debug("------------")

	proto.Walk(definition,
		proto.WithMessage(pbs.handleMessageTypeResolution),
		proto.WithService(pbs.handleServiceTypeResolution))

	proto.Walk(definition,
		proto.WithMessage(pbs.handleMessageBody),
		proto.WithService(pbs.handleServiceBody))

	if pbs.diveDepth == 0 {

		if len(selection) > 0 {
			if selection == "imports" {
				pbs.showDependencyTree()
			} else {
				pbs.showSelectedInclusion(selection)
			}
		} else {
			pbs.showInclusion(true, true)
		}

	} else {
		// this is not a root .proto file
	}
	return true
}

func processOneProto(name, selection string) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Println(".\n****** Recovered, ******\n\twhile processing", name, ", error: ", r)
		}
	}()

	pbs := NewPbs()
	process(pbs, name, selection)
	graphviz(pbs.outputFile, options(generateSvg), options(generatePng))
}

func applyToAllFiles(root, selection string) {

	trace("collecting all the .proto files from under " + root)
	var files []string
	if err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if strings.HasSuffix(path, ".proto") && strings.Index(path, "\\vendor\\") < 0 {
			files = append(files, path)
		}
		return nil
	}); err != nil {
		alert("there was an error", err)
		return
	}

	for _, file := range files {
		trace(".\n.\n===================== processing: ", file, "=====================")
		processOneProto(file, selection)
	}
}

func applyToAllFilesFromList(listfilename string, selection string) {
	file, err := os.Open(listfilename)
	if err != nil {
		assert("failed to open list: " + err.Error())
		return
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		name := scanner.Text()
		processOneProto(name, selection)
	}

	if err := scanner.Err(); err != nil {
		assert("failed to scan: " + err.Error())
		return
	}
}

const configDefaultName = "config.json"

var (
	g_configPath = flag.String("config", configDefaultName, "Location and name of the configuration file")
	g_logPath    = flag.String("log", "", "Location and name of the debug log file")
	g_source     = flag.String("src", "", "Location and name of the source file (required)")
	g_selection  = flag.String("select", "", "Name(s) of the selected elements")
	g_output     = flag.String("output", "", "Name of the output file")
	g_grpc       = flag.String("grpc", "", "Port to listen, e.g. :50051")
	g_action     = flag.String("action", "", "custom action to run upon completion (overwrites config.locations.action)")
)

//======================================================================================================================
func main() {

	if len(os.Args) == 1 {
		flag.Usage()
		return
	}

	for _, one := range os.Args {
		if one == "-install" {
			err := assets.ExtractAssets("", false)
			if err != nil {
				status("There was an error during the installation process:", err, "Please address these issues and repeat the installation process.")
			} else {

				// load config from assets
				// create dirs specified in the loaded config
				config, err := support.LoadConfig(assets.AssetUriPrefix+configDefaultName, false)
				if err == nil {
					for _, name := range []string{entryGenerated, "downloads"} {
						if dir, err := support.GetLocation(config, name); err == nil {
							createDirIfMissing(dir)
						}
					}
				}
			}
			return
		}
	}

	flag.Parse()

	config, err := support.LoadConfig(*g_configPath, (*g_configPath == configDefaultName))
	if err != nil {
		status("Error: failed to load config file")
		return
	}
	g_config = config

	if options("suppress all output") {
		g_debugLevel = debugNone
	}

	if len(*g_logPath) > 0 {
		log, err := os.Create(*g_logPath)
		if err == nil {
			defer log.Close()
			g_logWriter = log
		}
	}

	if len(*g_source) == 0 && len(*g_grpc) == 0 {
		status("No source file specified.")
		flag.Usage()
		return
	}

	if dir, err := support.GetLocation(config, entryGenerated); err == nil {
		createDirIfMissing(dir)
	}

	if len(*g_action) > 0 {
		support.SetLocation(g_config, "action", *g_action)
	}

	{ // preload templates
		aa := config["templates"].(map[string]interface{})
		tmplDir, err := support.GetLocation(config, "templates")
		if err != nil {
			tmplDir = ""
		}

		if err := plus.PreloadTemplates(aa, templFuncs, tmplDir); err != nil {
			status("failed to load templates", err)
			return
		}
	}

	if strings.HasPrefix(*g_source, "list:") {
		name := (*g_source)[5:]
		status("Processing the given list of the sources:", name)
		applyToAllFilesFromList(name, *g_selection)
		return
	}

	if len(*g_grpc) > 0 {
		err = grpc_main(*g_grpc)
		if err != nil {
			status("Failed to start daemon:", err)
		}
	} else {
		pbs := NewPbs()
		process(pbs, *g_source, *g_selection)
		graphviz(pbs.outputFile, options(generateSvg), options(generatePng))
	}
}
