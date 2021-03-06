package plugin

import (
	"fmt"
	"sort"
	"strings"

	"github.com/gogo/protobuf/protoc-gen-gogo/descriptor"
	"github.com/gogo/protobuf/protoc-gen-gogo/generator"
	jgorm "github.com/jinzhu/gorm"
	"github.com/jinzhu/inflection"

	"github.com/infobloxopen/protoc-gen-gorm/options"
)

const (
	typeMessage = 11
	typeEnum    = 14

	protoTypeTimestamp = "Timestamp" // last segment, first will be *google_protobufX
	protoTypeJSON      = "JSONValue"
	protoTypeUUID      = "UUIDValue"
)

// DB Engine Enum
const (
	ENGINE_UNSET = iota
	ENGINE_POSTGRES
)

var wellKnownTypes = map[string]string{
	"StringValue": "*string",
	"DoubleValue": "*double",
	"FloatValue":  "*float",
	"Int32Value":  "*int32",
	"Int64Value":  "*int64",
	"UInt32Value": "*uint32",
	"UInt64Value": "*uint64",
	"BoolValue":   "*bool",
	//  "BytesValue" : "*[]byte",
}

type OrmableType struct {
	Name   string
	Fields map[string]*Field
}

type Field struct {
	Type string
	*gorm.GormFieldOptions
}

func NewOrmableType() *OrmableType {
	return &OrmableType{Fields: make(map[string]*Field)}
}

// OrmPlugin implements the plugin interface and creates GORM code from .protos
type OrmPlugin struct {
	*generator.Generator
	dbEngine     int
	wktPkgName   string
	gormPkgName  string
	lftPkgName   string // 'Locally Famous Types', used for collection operators
	usingUUID    bool
	usingTime    bool
	usingAuth    bool
	usingJSON    bool
	ormableTypes map[string]*OrmableType
	EmptyFiles   []string
}

// Name identifies the plugin
func (p *OrmPlugin) Name() string {
	return "gorm"
}

// Init is called once after data structures are built but before
// code generation begins.
func (p *OrmPlugin) Init(g *generator.Generator) {
	p.Generator = g
	if strings.EqualFold(g.Param["engine"], "postgres") {
		p.dbEngine = ENGINE_POSTGRES
	} else {
		p.dbEngine = ENGINE_UNSET
	}
}

// Generate produces the code generated by the plugin for this file,
// except for the imports, by calling the generator's methods P, In, and Out.
func (p *OrmPlugin) Generate(file *generator.FileDescriptor) {
	p.resetImports()
	p.ormableTypes = make(map[string]*OrmableType)
	// Preload just the types we'll be creating
	for _, msg := range file.Messages() {
		// We don't want to bother with the MapEntry stuff
		if msg.GetOptions().GetMapEntry() {
			continue
		}
		typeName := generator.CamelCaseSlice(msg.TypeName())
		if getMessageOptions(msg).GetOrmable() {
			ormable := NewOrmableType()
			p.ormableTypes[typeName] = ormable
		}
	}
	for _, msg := range file.Messages() {
		typeName := generator.CamelCaseSlice(msg.TypeName())
		if p.isOrmable(typeName) {
			p.parseBasicFields(msg)
		}
	}
	for _, msg := range file.Messages() {
		typeName := generator.CamelCaseSlice(msg.TypeName())
		if p.isOrmable(typeName) {
			p.parseAssociations(msg)
		}
	}
	for _, msg := range file.Messages() {
		typeName := generator.CamelCaseSlice(msg.TypeName())
		if p.isOrmable(typeName) {
			p.generateOrmable(msg)
			p.generateTableNameFunction(msg)
			p.generateConvertFunctions(msg)
			p.generateHookInterfaces(msg)
		}
	}
	p.generateDefaultHandlers(file)
	p.generateDefaultServer(file)
	if len(p.ormableTypes) == 0 {
		p.EmptyFiles = append(p.EmptyFiles, file.GetName())
	}
}

func (p *OrmPlugin) parseBasicFields(msg *generator.Descriptor) {
	typeName := generator.CamelCaseSlice(msg.TypeName())
	ormable := p.getOrmable(typeName)
	ormable.Name = fmt.Sprintf("%sORM", typeName)
	for _, field := range msg.GetField() {
		fieldOpts := getFieldOptions(field)
		if fieldOpts.GetDrop() {
			continue
		}
		fieldName := generator.CamelCase(field.GetName())
		fieldType, _ := p.GoType(msg, field)
		if *(field.Type) == typeEnum {
			fieldType = "int32"
		} else if *(field.Type) != typeMessage && field.IsRepeated() {
			// Not implemented yet
			continue
		} else if *(field.Type) == typeMessage {
			//Check for WKTs or fields of nonormable types
			parts := strings.Split(fieldType, ".")
			rawType := parts[len(parts)-1]
			if v, exists := wellKnownTypes[rawType]; exists {
				p.RecordTypeUse(".google.protobuf.StringValue")
				p.wktPkgName = strings.Trim(parts[0], "*")
				fieldType = v
			} else if rawType == protoTypeUUID {
				fieldType = "*uuid.UUID"
				p.usingUUID = true
			} else if rawType == protoTypeTimestamp {
				p.usingTime = true
				fieldType = "time.Time"
			} else if rawType == protoTypeJSON {
				p.usingJSON = true
				if p.dbEngine == ENGINE_POSTGRES {
					fieldType = "*gormpq.Jsonb"
				} else {
					// Potential TODO: add types we want to use in other/default DB engine
					continue
				}
			} else {
				continue
			}
		}
		ormable.Fields[fieldName] = &Field{Type: fieldType, GormFieldOptions: fieldOpts}
	}
	if getMessageOptions(msg).GetMultiAccount() {
		if accID, ok := ormable.Fields["AccountID"]; !ok {
			ormable.Fields["AccountID"] = &Field{Type: "string"}
		} else {
			if accID.Type != "string" {
				p.Fail("Cannot include AccountID field into", ormable.Name, "as it already exists there with a different type.")
			}
		}
	}
	for _, field := range getMessageOptions(msg).GetInclude() {
		fieldName := generator.CamelCase(field.GetName())
		if _, ok := ormable.Fields[fieldName]; !ok {
			ormable.Fields[fieldName] = &Field{Type: field.GetType(), GormFieldOptions: &gorm.GormFieldOptions{Tag: field.GetTag()}}
		} else {
			p.Fail("Cannot include", fieldName, "field into", ormable.Name, "as it aready exists there.")
		}
	}
}

func (p *OrmPlugin) isOrmable(typeName string) bool {
	_, ok := p.ormableTypes[strings.Trim(typeName, "[]*")]
	return ok
}

func (p *OrmPlugin) getOrmable(typeName string) *OrmableType {
	if ormable, ok := p.ormableTypes[strings.Trim(typeName, "[]*")]; ok {
		return ormable
	} else {
		p.Fail(typeName, "is not ormable.")
		return nil
	}
}

func (p *OrmPlugin) getSortedFieldNames(fields map[string]*Field) []string {
	var keys []string
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (p *OrmPlugin) generateOrmable(message *generator.Descriptor) {
	ormable := p.getOrmable(p.TypeName(message))
	fields := getOrderedFieldNames(ormable, message)

	p.P(`type `, ormable.Name, ` struct {`)
	for _, fieldName := range fields {
		field := ormable.Fields[fieldName]
		p.P(fieldName, ` `, field.Type, p.renderGormTag(field))
	}
	p.P(`}`)
}

func (p *OrmPlugin) renderGormTag(field *Field) string {
	res := ""
	if tag := field.GetTag(); tag != nil {
		if tag.Column != nil {
			res += fmt.Sprintf("column:%s;", tag.GetColumn())
		}
		if tag.Type != nil {
			res += fmt.Sprintf("type:%s;", string(tag.GetType()))
		}
		if tag.Size_ != nil {
			res += fmt.Sprintf("size:%s;", string(tag.GetSize_()))
		}
		if tag.Precision != nil {
			res += fmt.Sprintf("precision:%s;", string(tag.GetPrecision()))
		}
		if tag.GetPrimaryKey() {
			res += "primary_key;"
		}
		if tag.GetUnique() {
			res += "unique;"
		}
		if tag.Default != nil {
			res += fmt.Sprintf("default:%s;", tag.GetDefault())
		}
		if tag.GetNotNull() {
			res += "not null;"
		}
		if tag.GetAutoIncrement() {
			res += "auto_increment;"
		}
		if tag.Index != nil {
			if tag.GetIndex() == "" {
				res += "index;"
			} else {
				res += fmt.Sprintf("index:%s;", tag.GetIndex())
			}
		}
		if tag.UniqueIndex != nil {
			if tag.GetUniqueIndex() == "" {
				res += "unique_index;"
			} else {
				res += fmt.Sprintf("unique_index:%s;", tag.GetUniqueIndex())
			}
		}
		if tag.GetEmbedded() {
			res += "embedded;"
		}
		if tag.EmbeddedPrefix != nil {
			res += fmt.Sprintf("embedded_prefix:%s;", tag.GetEmbeddedPrefix())
		}
		if tag.GetIgnore() {
			res += "-;"
		}
	}
	if hasOne := field.GetHasOne(); hasOne != nil {
		if hasOne.Foreignkey != nil {
			res += fmt.Sprintf("foreignkey:%s;", hasOne.GetForeignkey())
		}
		if hasOne.AssociationForeignkey != nil {
			res += fmt.Sprintf("association_foreignkey:%s;", hasOne.GetAssociationForeignkey())
		}
	} else if belongsTo := field.GetBelongsTo(); belongsTo != nil {
		if belongsTo.Foreignkey != nil {
			res += fmt.Sprintf("foreignkey:%s;", belongsTo.GetForeignkey())
		}
		if belongsTo.AssociationForeignkey != nil {
			res += fmt.Sprintf("association_foreignkey:%s;", belongsTo.GetAssociationForeignkey())
		}
	} else if hasMany := field.GetHasMany(); hasMany != nil {
		if hasMany.Foreignkey != nil {
			res += fmt.Sprintf("foreignkey:%s;", hasMany.GetForeignkey())
		}
		if hasMany.AssociationForeignkey != nil {
			res += fmt.Sprintf("association_foreignkey:%s;", hasMany.GetAssociationForeignkey())
		}
	} else if mtm := field.GetManyToMany(); mtm != nil {
		if mtm.Jointable != nil {
			res += fmt.Sprintf("many2many:%s;", mtm.GetJointable())
		}
		if mtm.Foreignkey != nil {
			res += fmt.Sprintf("foreignkey:%s;", mtm.GetForeignkey())
		}
		if mtm.AssociationForeignkey != nil {
			res += fmt.Sprintf("association_foreignkey:%s;", mtm.GetAssociationForeignkey())
		}
		if mtm.JointableForeignkey != nil {
			res += fmt.Sprintf("jointable_foreignkey:%s;", mtm.GetJointableForeignkey())
		}
		if mtm.AssociationJointableForeignkey != nil {
			res += fmt.Sprintf("association_jointable_foreignkey:%s;", mtm.GetAssociationJointableForeignkey())
		}
	}
	if res == "" {
		return ""
	}
	return fmt.Sprintf("`gorm:\"%s\"`", strings.TrimRight(res, ";"))
}

// generateTableNameFunction the function to set the gorm table name
// back to gorm default, removing "ORM" suffix
func (p *OrmPlugin) generateTableNameFunction(message *generator.Descriptor) {
	typeName := p.TypeName(message)

	p.P(`// TableName overrides the default tablename generated by GORM`)
	p.P(`func (`, typeName, `ORM) TableName() string {`)

	tableName := inflection.Plural(jgorm.ToDBName(message.GetName()))
	if opts := getMessageOptions(message); opts != nil && opts.Table != nil {
		tableName = opts.GetTable()
	}
	p.P(`return "`, tableName, `"`)
	p.P(`}`)
}

// generateMapFunctions creates the converter functions
func (p *OrmPlugin) generateConvertFunctions(message *generator.Descriptor) {
	typeName := p.TypeName(message)

	///// To Orm
	p.P(`// ToORM runs the BeforeToORM hook if present, converts the fields of this`)
	p.P(`// object to ORM format, runs the AfterToORM hook, then returns the ORM object`)
	p.P(`func (m *`, typeName, `) ToORM (ctx context.Context) (`, typeName, `ORM, error) {`)
	p.P(`to := `, typeName, `ORM{}`)
	p.P(`var err error`)
	p.P(`if prehook, ok := interface{}(m).(`, typeName, `WithBeforeToORM); ok {`)
	p.P(`if err = prehook.BeforeToORM(ctx, &to); err != nil {`)
	p.P(`return to, err`)
	p.P(`}`)
	p.P(`}`)
	for _, field := range message.GetField() {
		// Checking if field is skipped
		if getFieldOptions(field).GetDrop() {
			continue
		}
		p.generateFieldConversion(message, field, true)
	}
	if getMessageOptions(message).GetMultiAccount() {
		p.usingAuth = true
		p.P("accountID, err := auth.GetAccountID(ctx, nil)")
		p.P("if err != nil {")
		p.P("return to, err")
		p.P("}")
		p.P("to.AccountID = accountID")
	}
	p.P(`if posthook, ok := interface{}(m).(`, typeName, `WithAfterToORM); ok {`)
	p.P(`err = posthook.AfterToORM(ctx, &to)`)
	p.P(`}`)
	p.P(`return to, err`)
	p.P(`}`)

	p.P()
	///// To Pb
	p.P(`// ToPB runs the BeforeToPB hook if present, converts the fields of this`)
	p.P(`// object to PB format, runs the AfterToPB hook, then returns the PB object`)
	p.P(`func (m *`, typeName, `ORM) ToPB (ctx context.Context) (`,
		typeName, `, error) {`)
	p.P(`to := `, typeName, `{}`)
	p.P(`var err error`)
	p.P(`if prehook, ok := interface{}(m).(`, typeName, `WithBeforeToPB); ok {`)
	p.P(`if err = prehook.BeforeToPB(ctx, &to); err != nil {`)
	p.P(`return to, err`)
	p.P(`}`)
	p.P(`}`)
	for _, field := range message.GetField() {
		// Checking if field is skipped
		if getFieldOptions(field).GetDrop() {
			continue
		}
		p.generateFieldConversion(message, field, false)
	}
	p.P(`if posthook, ok := interface{}(m).(`, typeName, `WithAfterToPB); ok {`)
	p.P(`err = posthook.AfterToPB(ctx, &to)`)
	p.P(`}`)
	p.P(`return to, err`)
	p.P(`}`)
}

// Output code that will convert a field to/from orm.
func (p *OrmPlugin) generateFieldConversion(message *generator.Descriptor, field *descriptor.FieldDescriptorProto, toORM bool) error {
	fieldName := generator.CamelCase(field.GetName())
	fieldType, _ := p.GoType(message, field)
	if field.IsRepeated() { // Repeated Object ----------------------------------
		if p.isOrmable(fieldType) { // Repeated ORMable type
			//fieldType = strings.Trim(fieldType, "[]*")

			p.P(`for _, v := range m.`, fieldName, ` {`)
			p.P(`if v != nil {`)
			if toORM {
				p.P(`if temp`, fieldName, `, cErr := v.ToORM(ctx); cErr == nil {`)
			} else {
				p.P(`if temp`, fieldName, `, cErr := v.ToPB(ctx); cErr == nil {`)
			}
			p.P(`to.`, fieldName, ` = append(to.`, fieldName, `, &temp`, fieldName, `)`)
			p.P(`} else {`)
			p.P(`return to, cErr`)
			p.P(`}`)
			p.P(`} else {`)
			p.P(`to.`, fieldName, ` = append(to.`, fieldName, `, nil)`)
			p.P(`}`)
			p.P(`}`) // end repeated for
		} else {
			p.P(`// Repeated type `, fieldType, ` is not an ORMable message type`)
		}
	} else if *(field.Type) == typeEnum { // Singular Enum, which is an int32 ---
		if toORM {
			p.P(`to.`, fieldName, ` = int32(m.`, fieldName, `)`)
		} else {
			p.P(`to.`, fieldName, ` = `, fieldType, `(m.`, fieldName, `)`)
		}
	} else if *(field.Type) == typeMessage { // Singular Object -------------
		//Check for WKTs
		parts := strings.Split(fieldType, ".")
		coreType := parts[len(parts)-1]
		// Type is a WKT, convert to/from as ptr to base type
		if _, exists := wellKnownTypes[coreType]; exists { // Singular WKT -----
			if toORM {
				p.P(`if m.`, fieldName, ` != nil {`)
				p.P(`v := m.`, fieldName, `.Value`)
				p.P(`to.`, fieldName, ` = &v`)
				p.P(`}`)
			} else {
				p.P(`if m.`, fieldName, ` != nil {`)
				p.P(`to.`, fieldName, ` = &`, p.wktPkgName, ".", coreType,
					`{Value: *m.`, fieldName, `}`)
				p.P(`}`)
			}
		} else if coreType == protoTypeUUID { // Singular UUID type ------------
			if toORM {
				p.P(`if m.`, fieldName, ` != nil {`)
				p.P(`tempUUID, uErr := uuid.FromString(m.`, fieldName, `.Value)`)
				p.P(`if uErr != nil {`)
				p.P(`return to, uErr`)
				p.P(`}`)
				p.P(`to.`, fieldName, ` = &tempUUID`)
				p.P(`}`)
			} else {
				p.P(`if m.`, fieldName, ` != nil {`)
				p.P(`to.`, fieldName, ` = &gtypes.UUIDValue{Value: m.`, fieldName, `.String()}`)
				p.P(`}`)
			}
		} else if coreType == protoTypeTimestamp { // Singular WKT Timestamp ---
			if toORM {
				p.P(`if m.`, fieldName, ` != nil {`)
				p.P(`if to.`, fieldName, `, err = ptypes.Timestamp(m.`, fieldName, `); err != nil {`)
				p.P(`return to, err`)
				p.P(`}`)
				p.P(`}`)
			} else {
				p.P(`if to.`, fieldName, `, err = ptypes.TimestampProto(m.`, fieldName, `); err != nil {`)
				p.P(`return to, err`)
				p.P(`}`)
			}
		} else if coreType == protoTypeJSON {
			if p.dbEngine == ENGINE_POSTGRES {
				if toORM {
					p.P(`if m.`, fieldName, ` != nil {`)
					p.P(`to.`, fieldName, ` = &gormpq.Jsonb{[]byte(m.`, fieldName, `.Value)}`)
					p.P(`}`)
				} else {
					p.P(`if m.`, fieldName, ` != nil {`)
					p.P(`to.`, fieldName, ` = &gtypes.JSONValue{Value: string(m.`, fieldName, `.RawMessage)}`)
					p.P(`}`)
				}
			} // Potential TODO other DB engine handling if desired
		} else if p.isOrmable(fieldType) {
			// Not a WKT, but a type we're building converters for
			fieldType = strings.Trim(fieldType, "*")
			p.P(`if m.`, fieldName, ` != nil {`)
			if toORM {
				p.P(`temp`, fieldType, `, err := m.`, fieldName, `.ToORM (ctx)`)
			} else {
				p.P(`temp`, fieldType, `, err := m.`, fieldName, `.ToPB (ctx)`)
			}
			p.P(`if err != nil {`)
			p.P(`return to, err`)
			p.P(`}`)
			p.P(`to.`, fieldName, ` = &temp`, fieldType)
			p.P(`}`)
		}
	} else { // Singular raw ----------------------------------------------------
		p.P(`to.`, fieldName, ` = m.`, fieldName)
	}
	return nil
}

func (p *OrmPlugin) generateHookInterfaces(message *generator.Descriptor) {
	typeName := p.TypeName(message)
	p.P(`// The following are interfaces you can implement for special behavior during ORM/PB conversions`)
	p.P(`// of type `, typeName, ` the arg will be the target, the caller the one being converted from`)
	p.P()
	for _, desc := range [][]string{
		{"BeforeToORM", typeName + "ORM", " called before default ToORM code"},
		{"AfterToORM", typeName + "ORM", " called after default ToORM code"},
		{"BeforeToPB", typeName, " called before default ToPB code"},
		{"AfterToPB", typeName, " called after default ToPB code"},
	} {
		p.P(`// `, typeName, desc[0], desc[2])
		p.P(`type `, typeName, `With`, desc[0], ` interface {`)
		p.P(desc[0], `(context.Context, *`, desc[1], `) error`)
		p.P(`}`)
		p.P()
	}
}
