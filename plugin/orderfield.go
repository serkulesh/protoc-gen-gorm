package plugin

import (
	"github.com/gogo/protobuf/protoc-gen-gogo/generator"
)

// Return "orm fields" with structure as in message
func getOrderedFieldNames(ormable *OrmableType, message *generator.Descriptor) (fields []string) {

	// Iterate message fields and filtering by orm fields
	for _, v := range message.GetField() {
		fieldName := generator.CamelCase(*v.Name)
		_, ok := ormable.Fields[fieldName]
		if ok {
			fields = append(fields, fieldName)
		}
	}

	// Iterate ormableType fields, add (include & fields)
	for ormField := range ormable.Fields {
		if !searchField(fields, ormField) {
			fields = append(fields, ormField)
		}
	}

	return fields
}

func searchField(fields []string, searchName string) bool {
	for _, fieldName := range fields {
		if searchName == fieldName {
			return true
		}
	}
	return false
}
