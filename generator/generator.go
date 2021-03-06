// Copyright 2019 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package generator

import (
	"log"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/golang/protobuf/descriptor"
	"github.com/golang/protobuf/proto"
	dpb "github.com/golang/protobuf/protoc-gen-go/descriptor"
	"github.com/golang/protobuf/ptypes/empty"
	openapiv3 "github.com/googleapis/gnostic/openapiv3"
	surface_v1 "github.com/googleapis/gnostic/surface"
	"google.golang.org/genproto/googleapis/api/annotations"
)

var protoBufScalarTypes = getProtobufTypes()

// Gathers all symbolic references we generated in recursive calls.
var generatedSymbolicReferences = make(map[string]bool, 0)

// Gathers all messages that have been generated from symbolic references in recursive calls.
var generatedMessages = make(map[string]string, 0)

var shouldRenderEmptyImport = false

// Uses the output of gnostic to return a dpb.FileDescriptorSet (in bytes). 'renderer' contains
// the 'model' (surface model) which has all the relevant data to create the dpb.FileDescriptorSet.
// There are four main steps:
// 		1. buildDependencies to build all static FileDescriptorProto we need.
// 		2. buildSymbolicReferences 	recursively executes this plugin to generate all FileDescriptorSet based on symbolic
// 									references. A symbolic reference is an URL to another OpenAPI description inside of
//									current description.
//		3. buildMessagesFromTypes is called to create all messages which will be rendered in .proto
//		4. buildServiceFromMethods is called to create a RPC service which will be rendered in .proto
func (renderer *Renderer) runFileDescriptorSetGenerator() (fdSet *dpb.FileDescriptorSet, err error) {
	syntax := "proto3"
	n := renderer.Package + ".proto"

	// mainProto is the proto we ultimately want to render.
	mainProto := &dpb.FileDescriptorProto{
		Name:    &n,
		Package: &renderer.Package,
		Syntax:  &syntax,
	}
	fdSet = &dpb.FileDescriptorSet{
		File: []*dpb.FileDescriptorProto{mainProto},
	}

	buildDependencies(fdSet)
	err = buildSymbolicReferences(fdSet, renderer)
	if err != nil {
		return nil, err
	}

	err = buildMessagesFromTypes(mainProto, renderer)
	if err != nil {
		return nil, err
	}

	err = buildServiceFromMethods(mainProto, renderer)
	if err != nil {
		return nil, err
	}

	addDependencies(fdSet)

	return fdSet, err
}

// addDependencies adds the dependencies to the FileDescriptorProto we want to render (the last one). This essentially
// makes the 'import'  statements inside the .proto definition.
func addDependencies(fdSet *dpb.FileDescriptorSet) {
	// At last, we need to add the dependencies to the FileDescriptorProto in order to get them rendered.
	lastFdProto := getLast(fdSet.File)
	for _, fd := range fdSet.File {
		if fd != lastFdProto {
			if *fd.Name == "google/protobuf/empty.proto" { // Reference: https://github.com/googleapis/gnostic-grpc/issues/8
				if shouldRenderEmptyImport {
					lastFdProto.Dependency = append(lastFdProto.Dependency, *fd.Name)
				}
				continue
			}
			lastFdProto.Dependency = append(lastFdProto.Dependency, *fd.Name)
		}
	}
	// Sort imports so they will be rendered in a consistent order.
	sort.Strings(lastFdProto.Dependency)
}

// buildSymbolicReferences recursively generates all .proto definitions to external OpenAPI descriptions (URLs to other
// descriptions inside the current description).
func buildSymbolicReferences(fdSet *dpb.FileDescriptorSet, renderer *Renderer) (err error) {
	symbolicReferences := renderer.Model.SymbolicReferences
	symbolicReferences = trimAndRemoveDuplicates(symbolicReferences)

	symbolicFileDescriptorProtos := make([]*dpb.FileDescriptorProto, 0)
	for _, ref := range symbolicReferences {
		if _, alreadyGenerated := generatedSymbolicReferences[ref]; !alreadyGenerated {
			generatedSymbolicReferences[ref] = true

			// Lets get the standard gnostic output from the symbolic reference.
			cmd := exec.Command("gnostic", "--pb-out=-", ref)
			b, err := cmd.Output()
			if err != nil {
				return err
			}

			// Construct an OpenAPI document v3.
			document, err := createOpenAPIDocFromGnosticOutput(b)
			if err != nil {
				return err
			}

			// Create the surface model. Keep in mind that this resolves the references of the symbolic reference again!
			surfaceModel, err := surface_v1.NewModelFromOpenAPI3(document, ref)
			if err != nil {
				return err
			}

			// Prepare surface model for recursive call. TODO: Keep discovery documents in mind.
			inputDocumentType := "openapi.v3.Document"
			if document.Openapi == "2.0.0" {
				inputDocumentType = "openapi.v2.Document"
			}
			NewProtoLanguageModel().Prepare(surfaceModel, inputDocumentType)

			// Recursively call the generator.
			recursiveRenderer := NewRenderer(surfaceModel)
			fileName := path.Base(ref)
			recursiveRenderer.Package = strings.TrimSuffix(fileName, filepath.Ext(fileName))
			newFdSet, err := recursiveRenderer.runFileDescriptorSetGenerator()
			if err != nil {
				return err
			}
			renderer.SymbolicFdSets = append(renderer.SymbolicFdSets, newFdSet)

			symbolicProto := getLast(newFdSet.File)
			symbolicFileDescriptorProtos = append(symbolicFileDescriptorProtos, symbolicProto)
		}
	}

	fdSet.File = append(symbolicFileDescriptorProtos, fdSet.File...)
	return nil
}

// Protoreflect needs all the dependencies that are used inside of the FileDescriptorProto (that gets rendered)
// to work properly. Those dependencies are google/protobuf/empty.proto, google/api/annotations.proto,
// and "google/protobuf/descriptor.proto". For all those dependencies the corresponding
// FileDescriptorProto has to be added to the FileDescriptorSet. Protoreflect won't work
// if a reference is missing.
func buildDependencies(fdSet *dpb.FileDescriptorSet) {
	// Dependency to google/api/annotations.proto for gRPC-HTTP transcoding. Here a couple of problems arise:
	// 1. Problem: 	We cannot call descriptor.ForMessage(&annotations.E_Http), which would be our
	//				required dependency. However, we can call descriptor.ForMessage(&http) and
	//				then construct the extension manually.
	// 2. Problem: 	The name is set wrong.
	// 3. Problem: 	google/api/annotations.proto has a dependency to google/protobuf/descriptor.proto.
	http := annotations.Http{}
	fd, _ := descriptor.MessageDescriptorProto(&http)

	extensionName := "http"
	n := "google/api/annotations.proto"
	l := dpb.FieldDescriptorProto_LABEL_OPTIONAL
	t := dpb.FieldDescriptorProto_TYPE_MESSAGE
	tName := "google.api.HttpRule"
	extendee := ".google.protobuf.MethodOptions"

	httpExtension := &dpb.FieldDescriptorProto{
		Name:     &extensionName,
		Number:   &annotations.E_Http.Field,
		Label:    &l,
		Type:     &t,
		TypeName: &tName,
		Extendee: &extendee,
	}

	fd.Extension = append(fd.Extension, httpExtension)                        // 1. Problem
	fd.Name = &n                                                              // 2. Problem
	fd.Dependency = append(fd.Dependency, "google/protobuf/descriptor.proto") //3.rd Problem

	// Build other required dependencies
	e := empty.Empty{}
	fdp := dpb.DescriptorProto{}
	fd2, _ := descriptor.MessageDescriptorProto(&e)
	fd3, _ := descriptor.MessageDescriptorProto(&fdp)
	dependencies := []*dpb.FileDescriptorProto{fd, fd2, fd3}

	// According to the documentation of protoReflect.CreateFileDescriptorFromSet the file I want to print
	// needs to be at the end of the array. All other FileDescriptorProto are dependencies.
	fdSet.File = append(dependencies, fdSet.File...)
}

// buildMessagesFromTypes builds protobuf messages from the surface model types. If the type is a RPC request parameter
// the fields have to follow certain rules, and therefore have to be validated.
func buildMessagesFromTypes(descr *dpb.FileDescriptorProto, renderer *Renderer) (err error) {
	for _, t := range renderer.Model.Types {
		message := &dpb.DescriptorProto{}
		message.Name = &t.TypeName

		for i, f := range t.Fields {
			if isRequestParameter(t) {
				if f.Position == surface_v1.Position_PATH {
					validatePathParameter(f)
				}

				if f.Position == surface_v1.Position_QUERY {
					validateQueryParameter(f)
				}
			}
			if f.EnumValues != nil {
				message.EnumType = append(message.EnumType, buildEnumDescriptorProto(f))
			}

			ctr := int32(i + 1)
			fieldDescriptor := &dpb.FieldDescriptorProto{Number: &ctr}
			fieldDescriptor.Name = &f.FieldName
			fieldDescriptor.Type = getFieldDescriptorType(f.NativeType, f.EnumValues)
			setFieldDescriptorLabel(fieldDescriptor, f)
			setFieldDescriptorTypeName(fieldDescriptor, f, renderer.Package)

			// Maps are represented as nested types inside of the descriptor.
			if f.Kind == surface_v1.FieldKind_MAP {
				if strings.Contains(f.NativeType, "map[string][]") {
					// Not supported for now: https://github.com/LorenzHW/gnostic-grpc-deprecated/issues/3#issuecomment-509348357
					continue
				}
				mapDescriptorProto := buildMapDescriptorProto(f)
				fieldDescriptor.TypeName = mapDescriptorProto.Name
				message.NestedType = append(message.NestedType, mapDescriptorProto)
			}
			message.Field = append(message.Field, fieldDescriptor)
		}
		descr.MessageType = append(descr.MessageType, message)
		generatedMessages[*message.Name] = renderer.Package + "." + *message.Name
	}
	return nil
}

// buildServiceFromMethods builds a protobuf RPC service. For every method the corresponding gRPC-HTTP transcoding options (https://github.com/googleapis/googleapis/blob/master/google/api/http.proto)
// have to be set.
func buildServiceFromMethods(descr *dpb.FileDescriptorProto, renderer *Renderer) (err error) {
	methods := renderer.Model.Methods
	serviceName := findValidServiceName(descr.MessageType, strings.Title(renderer.Package))

	service := &dpb.ServiceDescriptorProto{
		Name: &serviceName,
	}
	descr.Service = []*dpb.ServiceDescriptorProto{service}

	for _, method := range methods {
		mOptionsDescr := &dpb.MethodOptions{}
		requestBody := getRequestBodyForRequestParameters(method.ParametersTypeName, renderer.Model.Types)
		httpRule := getHttpRuleForMethod(method, requestBody)
		if err := proto.SetExtension(mOptionsDescr, annotations.E_Http, &httpRule); err != nil {
			return err
		}

		if method.ParametersTypeName == "" {
			method.ParametersTypeName = "google.protobuf.Empty"
			shouldRenderEmptyImport = true
		}
		if method.ResponsesTypeName == "" {
			method.ResponsesTypeName = "google.protobuf.Empty"
			shouldRenderEmptyImport = true
		}

		mDescr := &dpb.MethodDescriptorProto{
			Name:       &method.HandlerName,
			InputType:  &method.ParametersTypeName,
			OutputType: &method.ResponsesTypeName,
			Options:    mOptionsDescr,
		}

		service.Method = append(service.Method, mDescr)
	}
	return nil
}

// buildEnumDescriptorProto builds the necessary descriptor to render a enum. (https://developers.google.com/protocol-buffers/docs/proto3#enum)
func buildEnumDescriptorProto(f *surface_v1.Field) *dpb.EnumDescriptorProto {
	enumDescriptor := &dpb.EnumDescriptorProto{Name: &f.NativeType}
	for enumCtr, value := range f.EnumValues {
		num := int32(enumCtr)
		name := strings.ToUpper(value)
		valueDescriptor := &dpb.EnumValueDescriptorProto{
			Name:   &name,
			Number: &num,
		}
		enumDescriptor.Value = append(enumDescriptor.Value, valueDescriptor)
	}
	return enumDescriptor
}

// buildMapDescriptorProto builds the necessary descriptor to render a map. (https://developers.google.com/protocol-buffers/docs/proto3#maps)
// A map is represented as nested message with two fields: 'key', 'value' and the Options set accordingly.
func buildMapDescriptorProto(field *surface_v1.Field) *dpb.DescriptorProto {
	isMapEntry := true
	n := field.FieldName + "Entry"

	mapDP := &dpb.DescriptorProto{
		Name:    &n,
		Field:   buildKeyValueFields(field),
		Options: &dpb.MessageOptions{MapEntry: &isMapEntry},
	}
	return mapDP
}

// buildKeyValueFields builds the necessary 'key', 'value' fields for the map descriptor.
func buildKeyValueFields(field *surface_v1.Field) []*dpb.FieldDescriptorProto {
	k, v := "key", "value"
	var n1, n2 int32 = 1, 2
	l := dpb.FieldDescriptorProto_LABEL_OPTIONAL
	t := dpb.FieldDescriptorProto_TYPE_STRING
	keyField := &dpb.FieldDescriptorProto{
		Name:   &k,
		Number: &n1,
		Label:  &l,
		Type:   &t,
	}

	valueType := field.NativeType[11:] // This transforms a string like 'map[string]int32' to 'int32'. In other words: the type of the value from the map.
	valueField := &dpb.FieldDescriptorProto{
		Name:     &v,
		Number:   &n2,
		Label:    &l,
		Type:     getFieldDescriptorType(valueType, field.EnumValues),
		TypeName: getTypeNameForMapValueType(valueType),
	}
	return []*dpb.FieldDescriptorProto{keyField, valueField}
}

// validatePathParameter validates if the path parameter has the requested structure.
// This is necessary according to: https://github.com/googleapis/googleapis/blob/master/google/api/http.proto#L62
func validatePathParameter(field *surface_v1.Field) {
	if field.Kind != surface_v1.FieldKind_SCALAR {
		log.Println("The path parameter with the Name " + field.Name + " is invalid. " +
			"The path template may refer to one or more fields in the gRPC request message, as" +
			" long as each field is a non-repeated field with a primitive (non-message) type. " +
			"See: https://github.com/googleapis/googleapis/blob/master/google/api/http.proto#L62 for more information.")
	}
}

// validateQueryParameter validates if the query parameter has the requested structure.
// This is necessary according to: https://github.com/googleapis/googleapis/blob/master/google/api/http.proto#L118
func validateQueryParameter(field *surface_v1.Field) {
	_, isScalar := protoBufScalarTypes[field.NativeType]
	if !(field.Kind == surface_v1.FieldKind_SCALAR ||
		(field.Kind == surface_v1.FieldKind_ARRAY && isScalar) ||
		(field.Kind == surface_v1.FieldKind_REFERENCE)) {
		log.Println("The query parameter with the Name " + field.Name + " is invalid. " +
			"Note that fields which are mapped to URL query parameters must have a primitive type or" +
			" a repeated primitive type or a non-repeated message type. " +
			"See: https://github.com/googleapis/googleapis/blob/master/google/api/http.proto#L118 for more information.")
	}

}

// isRequestParameter checks whether 't' is a type that will be used as a request parameter for a RPC method.
func isRequestParameter(t *surface_v1.Type) bool {
	if strings.Contains(t.Description, t.GetName()+" holds parameters to") {
		return true
	}
	return false
}

// setFieldDescriptorLabel sets a label for 'fd'. If it is an array we need the 'repeated' label.
func setFieldDescriptorLabel(fd *dpb.FieldDescriptorProto, f *surface_v1.Field) {
	label := dpb.FieldDescriptorProto_LABEL_OPTIONAL
	if f.Kind == surface_v1.FieldKind_ARRAY || strings.Contains(f.NativeType, "map") {
		label = dpb.FieldDescriptorProto_LABEL_REPEATED
	}
	fd.Label = &label
}

// setFieldDescriptorTypeName sets the TypeName of 'fd'. A TypeName has to be set if the field is a reference to another
// message. Otherwise it is nil. Names are set according to the protocol buffer style guide for message names:
// https://developers.google.com/protocol-buffers/docs/style#message-and-field-names
func setFieldDescriptorTypeName(fd *dpb.FieldDescriptorProto, f *surface_v1.Field, packageName string) {
	// A field with a type of Message always has a typeName associated with it (the name of the Message).
	if *fd.Type == dpb.FieldDescriptorProto_TYPE_MESSAGE {
		typeName := packageName + "." + f.NativeType

		// Check whether we generated this message already inside of another dependency. If so we will use that name instead.
		if n, ok := generatedMessages[f.NativeType]; ok {
			typeName = n
		}
		fd.TypeName = &typeName
	}
	if *fd.Type == dpb.FieldDescriptorProto_TYPE_ENUM {
		fd.TypeName = &f.NativeType
	}
}

// getRequestBodyForRequestParameters finds the corresponding surface model type for 'name' and returns the name of the
// field that is a request body. If no such field is found it returns nil.
func getRequestBodyForRequestParameters(name string, types []*surface_v1.Type) *string {
	requestParameterType := &surface_v1.Type{}

	for _, t := range types {
		if t.Name == name {
			requestParameterType = t
		}
	}

	for _, f := range requestParameterType.Fields {
		if f.Position == surface_v1.Position_BODY {
			return &f.FieldName
		}
	}
	return nil
}

// getHttpRuleForMethod constructs a HttpRule from google/api/http.proto. Enables gRPC-HTTP transcoding on 'method'.
// If not nil, body is also set.
func getHttpRuleForMethod(method *surface_v1.Method, body *string) annotations.HttpRule {
	var httpRule annotations.HttpRule
	switch method.Method {
	case "GET":
		httpRule = annotations.HttpRule{
			Pattern: &annotations.HttpRule_Get{
				Get: method.Path,
			},
		}
	case "POST":
		httpRule = annotations.HttpRule{
			Pattern: &annotations.HttpRule_Post{
				Post: method.Path,
			},
		}
	case "PUT":
		httpRule = annotations.HttpRule{
			Pattern: &annotations.HttpRule_Put{
				Put: method.Path,
			},
		}
	case "PATCH":
		httpRule = annotations.HttpRule{
			Pattern: &annotations.HttpRule_Patch{
				Patch: method.Path,
			},
		}
	case "DELETE":
		httpRule = annotations.HttpRule{
			Pattern: &annotations.HttpRule_Delete{
				Delete: method.Path,
			},
		}
	}

	if body != nil {
		httpRule.Body = *body
	}

	return httpRule
}

// getTypeNameForMapValueType returns the type name for the given 'valueType'.
// A type name for a field is only set if it is some kind of reference (non-scalar values) otherwise it is nil.
func getTypeNameForMapValueType(valueType string) *string {
	if _, ok := protoBufScalarTypes[valueType]; ok {
		return nil // Ok it is a scalar. For scalar values we don't set the TypeName of the field.
	}
	typeName := valueType
	return &typeName
}

// getFieldDescriptorType returns a field descriptor type for the given 'nativeType'. If it is not a scalar type
// then we have a reference to another type which will get rendered as a message.
func getFieldDescriptorType(nativeType string, enumValues []string) *dpb.FieldDescriptorProto_Type {
	protoType := dpb.FieldDescriptorProto_TYPE_MESSAGE
	if protoType, ok := protoBufScalarTypes[nativeType]; ok {
		return &protoType
	}
	if enumValues != nil {
		protoType := dpb.FieldDescriptorProto_TYPE_ENUM
		return &protoType
	}
	return &protoType
}

// createOpenAPIDocFromGnosticOutput uses the 'binaryInput' from gnostic to create a OpenAPI document.
func createOpenAPIDocFromGnosticOutput(binaryInput []byte) (*openapiv3.Document, error) {
	document := &openapiv3.Document{}
	err := proto.Unmarshal(binaryInput, document)
	if err != nil {
		// If we execute gnostic with argument: '-pb-out=-' we get an EOF. So lets only return other errors.
		if err.Error() != "unexpected EOF" {
			return nil, err
		}
	}
	return document, nil
}

// trimAndRemoveDuplicates returns a list of URLs that are not duplicates (considering only the part until the first '#')
func trimAndRemoveDuplicates(urls []string) []string {
	result := make([]string, 0)
	for _, url := range urls {
		parts := strings.Split(url, "#")
		if !isDuplicate(result, parts[0]) {
			result = append(result, parts[0])
		}
	}
	return result
}

// isDuplicate returns true if 's' is inside 'ss'.
func isDuplicate(ss []string, s string) bool {
	for _, s2 := range ss {
		if s == s2 {
			return true
		}
	}
	return false
}

// getLast returns the last FileDescriptorProto of the array 'protos'.
func getLast(protos []*dpb.FileDescriptorProto) *dpb.FileDescriptorProto {
	return protos[len(protos)-1]
}

// getProtobufTypes maps the .proto Type (given as string) (https://developers.google.com/protocol-buffers/docs/proto3#scalar)
// to the corresponding descriptor proto type.
func getProtobufTypes() map[string]dpb.FieldDescriptorProto_Type {
	typeMapping := make(map[string]dpb.FieldDescriptorProto_Type)
	typeMapping["double"] = dpb.FieldDescriptorProto_TYPE_DOUBLE
	typeMapping["float"] = dpb.FieldDescriptorProto_TYPE_FLOAT
	typeMapping["int64"] = dpb.FieldDescriptorProto_TYPE_INT64
	typeMapping["uint64"] = dpb.FieldDescriptorProto_TYPE_UINT64
	typeMapping["int32"] = dpb.FieldDescriptorProto_TYPE_INT32
	typeMapping["fixed64"] = dpb.FieldDescriptorProto_TYPE_FIXED64

	typeMapping["fixed32"] = dpb.FieldDescriptorProto_TYPE_FIXED32
	typeMapping["bool"] = dpb.FieldDescriptorProto_TYPE_BOOL
	typeMapping["string"] = dpb.FieldDescriptorProto_TYPE_STRING
	typeMapping["bytes"] = dpb.FieldDescriptorProto_TYPE_BYTES
	typeMapping["uint32"] = dpb.FieldDescriptorProto_TYPE_UINT32
	typeMapping["sfixed32"] = dpb.FieldDescriptorProto_TYPE_SFIXED32
	typeMapping["sfixed64"] = dpb.FieldDescriptorProto_TYPE_SFIXED64
	typeMapping["sint32"] = dpb.FieldDescriptorProto_TYPE_SINT32
	typeMapping["sint64"] = dpb.FieldDescriptorProto_TYPE_SINT64
	return typeMapping
}

// findValidServiceName finds a valid service name for the gRPC service. A valid service name is not already taken by a
// message. Reference: https://github.com/googleapis/gnostic-grpc/issues/7
func findValidServiceName(messages []*dpb.DescriptorProto, serviceName string) string {
	messageNames := make(map[string]bool)

	for _, m := range messages {
		messageNames[*m.Name] = true
	}

	validServiceName := serviceName
	ctr := 0
	for {
		if nameIsAlreadyTaken, _ := messageNames[validServiceName]; !nameIsAlreadyTaken {
			return validServiceName
		}
		validServiceName = serviceName + "Service"
		if ctr > 0 {
			validServiceName += strconv.Itoa(ctr)
		}
		ctr += 1
	}
}
