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

var getGlobalOperationWhiteList = sync.OnceValue(func() *operationWhiteList { return newOperationWhiteList() })

type operationWhiteList struct {
	operations map[string]struct{}
}

func (l *operationWhiteList) Contains(operation string) bool {
	if operation == "" {
		return false
	}
	_, exists := l.operations[operation]
	return exists
}

func newOperationWhiteList() *operationWhiteList {
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
					allowAnonymousOperations[parseFullMethodToOperation(method.FullName())] = struct{}{}
				}

			}
		}

		return true
	})

	return &operationWhiteList{
		operations: allowAnonymousOperations,
	}
}

// parseFullMethodToOperation turns "api.template.v1.Example.CreateExample" into the kratos operation
// "/api.template.v1.Example/CreateExample"; a name without a dot yields "".
func parseFullMethodToOperation(fullName protoreflect.FullName) string {
	name := string(fullName)
	index := strings.LastIndex(name, ".")
	if index < 0 {
		return ""
	}
	return fmt.Sprintf("/%s/%s", name[:index], name[index+1:])
}
