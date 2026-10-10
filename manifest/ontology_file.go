package manifest

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// OntologyFileDoc is the minimal shape of a kit ontology or extension YAML
// file needed to validate property types. It decodes ONLY the fields this
// validation needs — the many other fields a real ontology file carries
// (display_name, description, h3_config, indexes, embedding, category,
// cardinality, source_type/target_type, ...) are intentionally ignored (no
// KnownFields enforcement) so an ontology file validates for property-type
// correctness without the SDK having to model every field.
type OntologyFileDoc struct {
	Spec OntologyFileSpec `yaml:"spec"`
}

// OntologyFileSpec is the `spec:` block of an ontology/extension file.
type OntologyFileSpec struct {
	EntityTypes       []OntologyTypeDoc `yaml:"entity_types"`
	RelationshipTypes []OntologyTypeDoc `yaml:"relationship_types"`
}

// OntologyTypeDoc is an entity or relationship type carrying declared
// properties.
type OntologyTypeDoc struct {
	Name       string                `yaml:"name"`
	Properties []OntologyPropertyDoc `yaml:"properties"`
}

// OntologyPropertyDoc is a single declared property; only Name and Type are
// needed for property-type validation.
type OntologyPropertyDoc struct {
	Name string `yaml:"name"`
	Type string `yaml:"type"`
}

// ParseOntologyFile decodes an ontology/extension YAML file into the minimal
// type+property shape used for validation. Unknown fields are tolerated.
func ParseOntologyFile(data []byte) (*OntologyFileDoc, error) {
	var doc OntologyFileDoc
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse ontology file: %w", err)
	}
	return &doc, nil
}

// ValidateOntologyFile parses an ontology/extension YAML file and fails closed
// on the FIRST entity/relationship property whose declared type the platform
// cannot represent (OGA-569). It is the local mirror of the platform's
// install/upload-time check (oga-platform
// internal/domainkit.ValidateOntologyPropertyTypes): a kit author who runs
// this in their build/CI gets the same rejection they would hit at install,
// instead of shipping a kit whose numeric/geo/enum/array fields silently
// become strings.
func ValidateOntologyFile(data []byte) error {
	doc, err := ParseOntologyFile(data)
	if err != nil {
		return err
	}
	for i := range doc.Spec.EntityTypes {
		et := &doc.Spec.EntityTypes[i]
		for j := range et.Properties {
			if err := ValidatePropertyType(et.Name, et.Properties[j].Name, et.Properties[j].Type); err != nil {
				return err
			}
		}
	}
	for i := range doc.Spec.RelationshipTypes {
		rt := &doc.Spec.RelationshipTypes[i]
		for j := range rt.Properties {
			if err := ValidatePropertyType(rt.Name, rt.Properties[j].Name, rt.Properties[j].Type); err != nil {
				return err
			}
		}
	}
	return nil
}

// ValidateOntologyFilePath is a convenience wrapper over ValidateOntologyFile
// that reads the file from disk first. Useful in kit CI tests that iterate the
// declared ontology_files / extension_files.
func ValidateOntologyFilePath(path string) error {
	data, err := os.ReadFile(path) //nolint:gosec // path is a kit-author-controlled build input
	if err != nil {
		return fmt.Errorf("read ontology file %s: %w", path, err)
	}
	return ValidateOntologyFile(data)
}
