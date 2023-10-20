package tdf3

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/google/uuid"
)

var (
	ErrAttributeUnmarshal = Error("attribute unmarshal error")
	ErrAttributeMarshal   = Error("attribute marshal error")
	ErrAttributeParse     = Error("attribute parse error")
)

type Policy struct {
	UUID uuid.UUID `json:"uuid"`
	Body Body      `json:"body"`
}

type Body struct {
	DataAttributes []Attribute `json:"dataAttributes"`
	Dissem         []string    `json:"dissem"`
}

type Attribute struct {
	Authority string `json:"authority"`
	Name      string `json:"name"`
	Value     string `json:"value"`
}

type serializedAttr struct {
	Attribute string `json:"attribute"`
}

func (at *Attribute) UnmarshalJSON(data []byte) error {
	var serAt serializedAttr
	if err := json.Unmarshal(data, &serAt); err != nil {
		return errors.Join(ErrAttributeUnmarshal, err)
	}

	return at.ParseAttributeFromString(serAt.Attribute)
}

func (at *Attribute) MarshalJSON() ([]byte, error) {
	var serialization = serializedAttr{
		Attribute: fmt.Sprintf("%s/attr/%s/value/%s", at.Authority, url.PathEscape(at.Name), url.PathEscape(at.Value)),
	}

	if bytes, err := json.Marshal(serialization); err != nil {
		return nil, errors.Join(ErrAttributeMarshal, err)
	} else {
		return bytes, nil
	}
}

func (at *Attribute) ParseAttributeFromString(attr string) error {
	valStart := strings.LastIndex(attr, "/")

	if valStart == -1 {
		return fmt.Errorf("illegal url, couldn't find attribute value [%s]", attr)
	}

	attrVal := attr[valStart+1:]
	attrVal, err := url.PathUnescape(attrVal)
	if err != nil {
		return errors.Join(ErrAttributeParse, err)
	}

	idx := strings.LastIndex(attr[:valStart], "/")
	if attr[idx+1:valStart] != "value" {
		return fmt.Errorf("illegal attribute [%s], missing attribute value", attr)
	}

	nameStart := strings.LastIndex(attr[:idx], "/")
	if nameStart == -1 {
		return fmt.Errorf("illegal attribute [%s], missing attribute name", attr)
	}

	attrName := attr[nameStart+1 : idx]
	attrName, err = url.PathUnescape(attrName)
	if err != nil {
		return errors.Join(ErrAttributeParse, err)
	}
	idx = strings.LastIndex(attr[:nameStart], "/")
	if idx == -1 {
		return fmt.Errorf("illegal attribute [%s], missing `attr`", attr)
	}

	if attr[idx+1:nameStart] != "attr" {
		return fmt.Errorf("illegal attribute [%s], missing `attr`", attr[idx+1:nameStart])
	}

	authority := attr[:idx]

	at.Authority = authority
	at.Name = attrName
	at.Value = attrVal

	return nil
}
