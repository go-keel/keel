// Package thirdparty holds the third-party .proto files keel imports or prescribes (copied by
// scripts/sync-third-party.sh) and the test that keeps them, and Keel's own .proto files, in step with the Go packages
// that are actually linked.
package thirdparty_test

import (
	"context"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bufbuild/protocompile"
	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"

	_ "buf.build/gen/go/bufbuild/protovalidate/protocolbuffers/go/buf/validate"
	_ "google.golang.org/genproto/googleapis/api/annotations"
	_ "google.golang.org/genproto/googleapis/api/httpbody"

	_ "github.com/go-keel/keel/conf"
	_ "github.com/go-keel/keel/proto/keel/v1"
)

// roots are the Buf workspace modules (see the root buf.yaml); each is an import root.
var roots = []string{"../proto", "../conf", "protovalidate", "googleapis"}

// TestThirdPartyMatchesGoPackages compiles every .proto file in the workspace and compares it with the descriptor the
// linked Go package registered under the same path. A mismatch means a rule written in .proto is not what the runtime
// reads, e.g. a validation rule the linked protovalidate version does not know and would silently skip.
func TestThirdPartyMatchesGoPackages(t *testing.T) {
	var files []string
	for _, root := range roots {
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".proto") {
				return err
			}
			rel, err := filepath.Rel(root, p)
			files = append(files, filepath.ToSlash(rel))
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	c := protocompile.Compiler{Resolver: protocompile.WithStandardImports(&protocompile.SourceResolver{ImportPaths: roots})}
	compiled, err := c.Compile(context.Background(), files...)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range compiled {
		path := f.Path()
		reg, err := protoregistry.GlobalFiles.FindFileByPath(path)
		if err != nil {
			t.Errorf("%s: no Go package registers this path (%v). Either the file sits at the wrong path, or the Go package "+
				"that registers it is not imported by this test", path, err)
			continue
		}
		got, want := normalize(t, f), normalize(t, reg)
		if !proto.Equal(got, want) {
			t.Errorf("%s differs from the descriptor registered by its Go package. For a third-party file, change the "+
				"pinned version in scripts/sync-third-party.sh to the upstream commit matching go.mod, then run make proto; "+
				"for Keel's own file, run make proto / make config.\n%s", path, firstDiff(got, want))
		}
	}
}

// firstDiff shows a few lines around the first difference; full descriptors run to thousands of lines.
func firstDiff(got, want *descriptorpb.FileDescriptorProto) string {
	o := prototext.MarshalOptions{Multiline: true}
	g, w := strings.Split(o.Format(got), "\n"), strings.Split(o.Format(want), "\n")
	i := 0
	for i < len(g) && i < len(w) && g[i] == w[i] {
		i++
	}
	from, to := max(0, i-3), i+3
	return "compiled (.proto):\n" + strings.Join(g[from:min(to, len(g))], "\n") +
		"\nregistered (Go package):\n" + strings.Join(w[from:min(to, len(w))], "\n")
}

// normalize drops source info (comments, positions) and re-parses with the global type registry: the compiler stores
// message-typed custom options as dynamic messages while the registered side uses generated types, so proto.Equal
// would report a difference even when the bytes are identical.
func normalize(t *testing.T, fd protoreflect.FileDescriptor) *descriptorpb.FileDescriptorProto {
	t.Helper()
	p := protodesc.ToFileDescriptorProto(fd)
	p.SourceCodeInfo = nil
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	out := &descriptorpb.FileDescriptorProto{}
	if err := (proto.UnmarshalOptions{Resolver: protoregistry.GlobalTypes}).Unmarshal(b, out); err != nil {
		t.Fatal(err)
	}
	return out
}
