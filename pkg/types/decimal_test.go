package types_test

import (
	"testing"

	"buf.build/gen/go/bufbuild/protovalidate/protocolbuffers/go/buf/validate"
	"buf.build/go/protovalidate"
	"github.com/go-keel/keel/pkg/types"
	keelpb "github.com/go-keel/keel/proto/keel/v1"
	"github.com/shopspring/decimal"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

func TestDecimalString(t *testing.T) {
	tests := []struct {
		name     string
		input    decimal.Decimal
		expected string
	}{
		{"integer", decimal.NewFromInt(100), "100"},
		{"fraction", decimal.RequireFromString("123.45"), "123.45"},
		{"zero", decimal.Zero, "0"},
		{"negative", decimal.RequireFromString("-99.50"), "-99.5"},
		{"more than 8 decimal places output exactly", decimal.RequireFromString("0.123456789"), "0.123456789"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := types.DecimalString(tt.input); got != tt.expected {
				t.Errorf("DecimalString() = %v, expected %v", got, tt.expected)
			}
		})
	}
}

func TestParseDecimal(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantValue string
		wantErr   bool
	}{
		{"valid value", "123.45", "123.45", false},
		{"zero", "0", "0", false},
		{"empty string", "", "0", true},
		{"invalid format", "abc", "0", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := types.ParseDecimal(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseDecimal() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got.String() != tt.wantValue {
				t.Errorf("ParseDecimal() = %v, want %v", got.String(), tt.wantValue)
			}
		})
	}
}

func TestDecimalOrZero(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"valid value", "123.45", "123.45"},
		{"empty string returns Zero", "", "0"},
		{"invalid format returns Zero", "invalid", "0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := types.DecimalOrZero(tt.input); got.String() != tt.expected {
				t.Errorf("DecimalOrZero() = %v, want %v", got.String(), tt.expected)
			}
		})
	}
}

// decimalMessage builds a message type: price is a plain string field and discount is optional, both carrying the
// `keel.v1.decimal` rule. The descriptor is built at run time rather than written in a .proto: Keel does not create contract files for tests.
func decimalMessage(t *testing.T) protoreflect.MessageDescriptor {
	t.Helper()
	rules := &validate.FieldRules{Type: &validate.FieldRules_String_{String_: &validate.StringRules{}}}
	proto.SetExtension(rules.GetString(), keelpb.E_Decimal, true)
	opts := &descriptorpb.FieldOptions{}
	proto.SetExtension(opts, validate.E_Field, rules)

	str := descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()
	label := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()
	fd, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
		Name:       proto.String("keel/test/decimal.proto"),
		Package:    proto.String("keel.test"),
		Syntax:     proto.String("proto3"),
		Dependency: []string{"buf/validate/validate.proto", "keel/v1/rules.proto"},
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("Req"),
			Field: []*descriptorpb.FieldDescriptorProto{
				{Name: proto.String("price"), Number: proto.Int32(1), Type: str, Label: label, Options: opts},
				{
					Name: proto.String("discount"), Number: proto.Int32(2), Type: str, Label: label, Options: opts,
					Proto3Optional: proto.Bool(true), OneofIndex: proto.Int32(0),
				},
			},
			OneofDecl: []*descriptorpb.OneofDescriptorProto{{Name: proto.String("_discount")}},
		}},
	}, protoregistry.GlobalFiles)
	if err != nil {
		t.Fatal(err)
	}
	return fd.Messages().Get(0)
}

// The rule's bounds are a public promise: loosening any of them lets numbers beyond DECIMAL(28, 8) pass validation;
// every accepted value must be parseable by ParseDecimal.
func TestDecimalRuleBoundaries(t *testing.T) {
	v, err := protovalidate.New()
	if err != nil {
		t.Fatal(err)
	}
	md := decimalMessage(t)
	price := md.Fields().ByName("price")

	valid := []string{
		"0", "123", "123.45", "0.01", "-99.50", "-0", "-1000000",
		"99999999999999999999.99999999", // 20 integer digits + 8 decimal places
	}
	for _, s := range valid {
		m := dynamicpb.NewMessage(md)
		m.Set(price, protoreflect.ValueOfString(s))
		if err := v.Validate(m); err != nil {
			t.Errorf("value %q should pass validation: %v", s, err)
		}
		if _, err := types.ParseDecimal(s); err != nil {
			t.Errorf("value %q passes the rule but ParseDecimal fails: %v", s, err)
		}
	}

	invalid := []string{
		"", "abc", "--5", "1.2.3", "01", "+1", "1e3", "100.", ".5", " 1",
		"1.999999999",             // 9 decimal places
		"100000000000000000000.1", // 21 integer digits
	}
	for _, s := range invalid {
		m := dynamicpb.NewMessage(md)
		m.Set(price, protoreflect.ValueOfString(s))
		if err := v.Validate(m); err == nil {
			t.Errorf("value %q should fail validation", s)
		}
	}
}

// Whether it is required depends on optional: an optional field may be omitted, but if sent it must be valid, and an empty string is rejected too.
func TestDecimalRuleOptionalField(t *testing.T) {
	v, err := protovalidate.New()
	if err != nil {
		t.Fatal(err)
	}
	md := decimalMessage(t)
	price, discount := md.Fields().ByName("price"), md.Fields().ByName("discount")

	for _, tt := range []struct {
		name     string
		discount *string
		ok       bool
	}{
		{"omitted", nil, true},
		{"valid value", proto.String("0.5"), true},
		{"empty string", proto.String(""), false},
		{"invalid value", proto.String("abc"), false},
	} {
		m := dynamicpb.NewMessage(md)
		m.Set(price, protoreflect.ValueOfString("1"))
		if tt.discount != nil {
			m.Set(discount, protoreflect.ValueOfString(*tt.discount))
		}
		if err := v.Validate(m); (err == nil) != tt.ok {
			t.Errorf("%s: err = %v, want ok = %v", tt.name, err, tt.ok)
		}
	}
}
