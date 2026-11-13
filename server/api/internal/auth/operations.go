package auth

import (
	"fmt"
	"strings"
	"sync"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"

	keelpb "github.com/go-keel/keel/proto/keel/v1"
)

var getGlobalOperationWhiteList = sync.OnceValue(func() *OperationWhiteList { return newOperationWhiteList() })

type OperationWhiteList struct {
	operations map[string]struct{}
}

func (l *OperationWhiteList) Contains(operation string) bool {
	if operation == "" {
		return false
	}
	_, exists := l.operations[operation]
	return exists
}

func newOperationWhiteList() *OperationWhiteList {
	allowAnonymousOperations := make(map[string]struct{})

	files := protoregistry.GlobalFiles

	files.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		if fd.Services().Len() == 0 {
			return true
		}

		for i := 0; i < fd.Services().Len(); i++ {
			service := fd.Services().Get(i)

			for j := 0; j < service.Methods().Len(); j++ {
				method := service.Methods().Get(j)

				opts := method.Options().(*descriptorpb.MethodOptions)
				authRule := proto.GetExtension(opts, keelpb.E_Auth).(*keelpb.AuthRule)
				if authRule != nil && authRule.AllowAnonymous {
					operation, _, _ := parseFullMethodToOperation(method.FullName())
					allowAnonymousOperations[operation] = struct{}{}
				}

			}
		}

		return true
	})

	return &OperationWhiteList{
		operations: allowAnonymousOperations,
	}
}

func parseFullMethodToOperation(fullName protoreflect.FullName) (operation string, service string, method string) {
	// "api.template.v1.Example.CreateExample"
	name := string(fullName)
	index := strings.LastIndex(name, ".")
	if index < 0 {
		return "", "", ""
	}

	// api.template.v1.Example
	service = name[:index]
	// CreateExample
	method = name[index+1:]
	// /api.template.v1.Example/CreateExample
	operation = fmt.Sprintf("/%s/%s", service, method)
	return
}
