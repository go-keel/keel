// In-package: checks slot placement and the validate middleware's message families, not visible through the public API.

package mw

import (
	"context"
	"strings"
	"testing"

	"buf.build/gen/go/bufbuild/protovalidate/protocolbuffers/go/buf/validate"
	"buf.build/go/protovalidate"
	kerrors "github.com/go-kratos/kratos/v3/errors"
	"github.com/nicksnyder/go-i18n/v2/i18n"
	"golang.org/x/text/language"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/go-keel/keel/middleware/metadata"
	"github.com/go-keel/keel/middleware/sentinel"
	keelpb "github.com/go-keel/keel/proto/keel/v1"
)

func TestDefault_SlotOrder(t *testing.T) {
	chain := Default(nil)
	want := []string{SlotRecovery, SlotTracing, SlotMetrics, SlotLogging, SlotMetadata, SlotAuth, SlotValidate}
	if len(chain.slots) != len(want) {
		t.Fatalf("slots = %d, want %d", len(chain.slots), len(want))
	}
	for i, s := range chain.slots {
		if s.name != want[i] {
			t.Errorf("slot[%d] = %q, want %q", i, s.name, want[i])
		}
	}
}

func TestWithRatelimit_InsertsAfterRecovery(t *testing.T) {
	chain := Default(nil)
	WithRatelimit(chain)
	if chain.slots[0].name != SlotRecovery || chain.slots[1].name != SlotRatelimit {
		t.Errorf("ratelimit should sit right after recovery, got %q,%q",
			chain.slots[0].name, chain.slots[1].name)
	}
}

// A missing bundle fails startup through the chain error instead of installing a locale middleware that panics on
// every request.
func TestWithLocale_MissingBundleRecordsError(t *testing.T) {
	chain := Default(nil)
	WithLocale(chain, nil)
	wantRule(t, chain.Err(), "mw.locale-bundle-missing")
	if hasSlot(chain.slots, SlotLocale) {
		t.Fatal("locale slot mounted for a missing bundle, want none")
	}
}

func TestWithSentinel_NilGroupRecordsError(t *testing.T) {
	chain := Default(nil)
	WithSentinel(chain, nil)
	wantRule(t, chain.Err(), "mw.sentinel-group-missing")
}

// Registration errors recorded by the group reach the chain, so the server constructor returns them at startup.
func TestWithSentinel_JoinsGroupErrors(t *testing.T) {
	sg := sentinel.NewGroup().Default(nil)
	chain := Default(nil)
	WithSentinel(chain, sg)
	wantRule(t, chain.Err(), "sentinel.rule-invalid")
}

// Mounting a preset twice records an error and leaves one copy, rather than running the middleware twice.
func TestPresetMountedTwiceRecordsError(t *testing.T) {
	cases := map[string]func(*Chain){
		"WithSentinel":  func(c *Chain) { WithSentinel(c, sentinel.NewGroup()) },
		"WithRatelimit": func(c *Chain) { WithRatelimit(c) },
		"WithLocale":    func(c *Chain) { WithLocale(c, i18n.NewBundle(language.English)) },
	}
	for name, mount := range cases {
		t.Run(name, func(t *testing.T) {
			chain := Default(nil)
			mount(chain)
			if chain.Err() != nil {
				t.Fatalf("first mount: Err() = %v, want nil", chain.Err())
			}
			n := len(chain.slots)
			mount(chain)
			wantRule(t, chain.Err(), "mw.slot-duplicate")
			if len(chain.slots) != n {
				t.Fatalf("slots = %d after second mount, want %d", len(chain.slots), n)
			}
		})
	}
}

// wantRule fails unless err carries the Keel rule id.
func wantRule(t *testing.T, err error, id string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "keel/"+id+":") {
		t.Fatalf("err = %v, want rule %s", err, id)
	}
}

// decimalRequest builds a request whose only string field, price, carries the `keel.v1.decimal` rule. The descriptor
// is built at run time rather than written in a .proto: Keel does not create contract files for tests.
func decimalRequest(t *testing.T, price string) proto.Message {
	t.Helper()
	rules := &validate.FieldRules{Type: &validate.FieldRules_String_{String_: &validate.StringRules{}}}
	proto.SetExtension(rules.GetString(), keelpb.E_Decimal, true)
	opts := &descriptorpb.FieldOptions{}
	proto.SetExtension(opts, validate.E_Field, rules)

	fd, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
		Name:       proto.String("keel/test/mw.proto"),
		Package:    proto.String("keel.test"),
		Syntax:     proto.String("proto3"),
		Dependency: []string{"buf/validate/validate.proto", "keel/v1/rules.proto"},
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("Req"),
			Field: []*descriptorpb.FieldDescriptorProto{{
				Name:    proto.String("price"),
				Number:  proto.Int32(1),
				Type:    descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
				Label:   descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
				Options: opts,
			}},
		}},
	}, protoregistry.GlobalFiles)
	if err != nil {
		t.Fatal(err)
	}
	md := fd.Messages().Get(0)
	m := dynamicpb.NewMessage(md)
	m.Set(md.Fields().ByName("price"), protoreflect.ValueOfString(price))
	return m
}

// Anchors the encoding contract: a validation failure must be BadRequest(400)/reason VALIDATOR with the cause kept;
// downstream sentinel and error encoding depend on that shape. The default chain's validator takes no options, and
// Keel's own shared rules (rules.proto) must take effect here too.
func TestDefaultChain_ValidateErrorShape(t *testing.T) {
	fn, err := protovalidateFunc()
	if err != nil {
		t.Fatal(err)
	}
	h := validateMiddleware(fn)(func(ctx context.Context, req any) (any, error) { return "ok", nil })

	_, err = h(context.Background(), decimalRequest(t, "abc"))
	if err == nil {
		t.Fatal("invalid decimal should be rejected")
	}
	kerr := kerrors.FromError(err)
	if kerr.GetCode() != 400 || kerr.GetReason() != "VALIDATOR" {
		t.Fatalf("code=%d reason=%s, want 400/VALIDATOR", kerr.GetCode(), kerr.GetReason())
	}
	if kerr.Unwrap() == nil {
		t.Fatal("cause should be preserved")
	}
}

// fieldRequest builds a request with one standard rule each on name (max 3), age (>=18), mail (email) and must (required).
func fieldRequest(t *testing.T, name string, age int32, mail, must string) proto.Message {
	t.Helper()
	field := func(n string, num int32, typ descriptorpb.FieldDescriptorProto_Type, r *validate.FieldRules) *descriptorpb.FieldDescriptorProto {
		opts := &descriptorpb.FieldOptions{}
		proto.SetExtension(opts, validate.E_Field, r)
		return &descriptorpb.FieldDescriptorProto{
			Name: proto.String(n), Number: proto.Int32(num), Type: typ.Enum(),
			Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Options: opts,
		}
	}
	str := func(r *validate.StringRules) *validate.FieldRules {
		return &validate.FieldRules{Type: &validate.FieldRules_String_{String_: r}}
	}
	fd, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
		Name: proto.String("keel/test/fields.proto"), Package: proto.String("keel.test"),
		Syntax: proto.String("proto3"), Dependency: []string{"buf/validate/validate.proto"},
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("FieldReq"),
			Field: []*descriptorpb.FieldDescriptorProto{
				field("name", 1, descriptorpb.FieldDescriptorProto_TYPE_STRING, str(&validate.StringRules{MaxLen: proto.Uint64(3)})),
				field("age", 2, descriptorpb.FieldDescriptorProto_TYPE_INT32, &validate.FieldRules{
					Type: &validate.FieldRules_Int32{Int32: &validate.Int32Rules{GreaterThan: &validate.Int32Rules_Gte{Gte: 18}}},
				}),
				field("mail", 3, descriptorpb.FieldDescriptorProto_TYPE_STRING, str(&validate.StringRules{WellKnown: &validate.StringRules_Email{Email: true}})),
				field("must", 4, descriptorpb.FieldDescriptorProto_TYPE_STRING, &validate.FieldRules{Required: proto.Bool(true)}),
			},
		}},
	}, protoregistry.GlobalFiles)
	if err != nil {
		t.Fatal(err)
	}
	md := fd.Messages().Get(0)
	m := dynamicpb.NewMessage(md)
	m.Set(md.Fields().ByName("name"), protoreflect.ValueOfString(name))
	m.Set(md.Fields().ByName("age"), protoreflect.ValueOfInt32(age))
	m.Set(md.Fields().ByName("mail"), protoreflect.ValueOfString(mail))
	m.Set(md.Fields().ByName("must"), protoreflect.ValueOfString(must))
	return m
}

// The message is protovalidate's own text, as kratos's validate middleware returns it: it names the field and the
// constraint, so the developer reading it knows what to fix, and it is the same whatever language the request asks for.
func TestValidateMessage_NamesFieldAndConstraint(t *testing.T) {
	v, err := protovalidate.New()
	if err != nil {
		t.Fatal(err)
	}
	h := validateMiddleware(func(x any) error { return v.Validate(x.(proto.Message)) })(
		func(ctx context.Context, req any) (any, error) { return "ok", nil })
	ctx, err := metadata.Set(context.Background(), metadata.KeyAcceptLanguage, "zh-CN")
	if err != nil {
		t.Fatal(err)
	}
	_, err = h(ctx, fieldRequest(t, "toolong", 20, "a@b.co", "x"))
	msg := kerrors.FromError(err).GetMessage()
	if !strings.Contains(msg, "name: ") || !strings.Contains(msg, "at most 3") {
		t.Fatalf("message %q should name the field and the constraint", msg)
	}
}
