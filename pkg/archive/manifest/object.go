package manifest

import "github.com/arkavo/backend-go/pkg/tdf3"

type Object struct {
	EncryptionInformation tdf3.EncryptionInformation `json:"encryptionInformation"`
	Payload               Payload                    `json:"payload"`
	SchemaVersion         string                     `json:"schemaVersion,omitempty"`
}

type Payload struct {
	IsEncrypted bool   `json:"isEncrypted"`
	MimeType    string `json:"mimeType"`
	Protocol    string `json:"protocol"`
	Type        string `json:"type"`
	URL         string `json:"url"`
}
