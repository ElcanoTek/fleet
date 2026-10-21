// Bounded decoding for the approval decision bodies.
//
// The approve POSTs carry client-sized lists: the group decision's approve and
// decline ids, its narrow map and narrow_args, and the single-card
// approved_values. Their bodies are capped in bytes (maxApprovalBody,
// maxApprovalGroupBody) so that every decision the endpoints accept fits, but
// a body of that size can hold millions of empty or one-character strings,
// and encoding/json would materialize every one (hundreds of megabytes)
// before a count check after decoding could refuse it. These types read
// their lists token by token instead and refuse the request as soon as a
// count passes its bound, so what a refused request allocates is bounded by
// the limits, not by the body. Their errors never quote the client's values.

package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// approvalIDList is a group decision's approve or decline list: at most
// maxApprovalGroupDecisions ids, refused while decoding past that.
type approvalIDList []string

// UnmarshalJSON reads the list with its length bounded as it goes.
func (l *approvalIDList) UnmarshalJSON(b []byte) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	values, err := decodeBoundedStrings(dec, maxApprovalGroupDecisions, errTooManyGroupDecisions)
	if err != nil {
		return err
	}
	*l = values
	return nil
}

// approvalGroupNarrow is a group decision's narrow map: at most
// maxApprovalGroupDecisions cards, and at most maxApprovalGroupNarrowedValues
// values across them, refused while decoding past either.
type approvalGroupNarrow map[string][]string

// UnmarshalJSON reads the map with its key and value counts bounded as it
// goes. A null entry decodes to a nil list (checkGroupNarrowing refuses it).
func (m *approvalGroupNarrow) UnmarshalJSON(b []byte) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	isNull, err := openBounded(dec, '{')
	if err != nil || isNull {
		*m = nil
		return err
	}
	out := approvalGroupNarrow{}
	total := 0
	for dec.More() {
		if len(out) >= maxApprovalGroupDecisions {
			return errTooManyGroupDecisions
		}
		key, err := objectKey(dec)
		if err != nil {
			return err
		}
		values, err := decodeBoundedStrings(dec, maxApprovalGroupNarrowedValues-total, errTooManyGroupNarrowedValues)
		if err != nil {
			return err
		}
		total += len(values)
		out[key] = values
	}
	if err := closeBounded(dec, '}'); err != nil {
		return err
	}
	*m = out
	return nil
}

// approvalGroupNarrowArgs is a group decision's narrow_args map: at most
// maxApprovalGroupDecisions entries, refused while decoding past that.
type approvalGroupNarrowArgs map[string]string

// UnmarshalJSON reads the map with its size bounded as it goes.
func (m *approvalGroupNarrowArgs) UnmarshalJSON(b []byte) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	isNull, err := openBounded(dec, '{')
	if err != nil || isNull {
		*m = nil
		return err
	}
	out := approvalGroupNarrowArgs{}
	for dec.More() {
		if len(out) >= maxApprovalGroupDecisions {
			return errTooManyGroupDecisions
		}
		key, err := objectKey(dec)
		if err != nil {
			return err
		}
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		value, ok := tok.(string)
		if !ok {
			return errors.New("narrow_args values must be strings")
		}
		out[key] = value
	}
	if err := closeBounded(dec, '}'); err != nil {
		return err
	}
	*m = out
	return nil
}

var (
	errTooManyGroupDecisions      = fmt.Errorf("too many approvals in one group decision (max %d)", maxApprovalGroupDecisions)
	errTooManyGroupNarrowedValues = fmt.Errorf("too many narrowed values in one group decision (max %d); decide the cards one at a time", maxApprovalGroupNarrowedValues)
	errTooManyApprovedValues      = fmt.Errorf("approved_values names more than %d values, more than any card stages", maxNarrowableValues)
)

// decodeBoundedStrings reads one JSON value from dec that must be null (a
// nil list) or an array of strings, refusing with tooMany as soon as it holds
// more than max of them.
func decodeBoundedStrings(dec *json.Decoder, maxValues int, tooMany error) ([]string, error) {
	isNull, err := openBounded(dec, '[')
	if err != nil || isNull {
		return nil, err
	}
	out := []string{}
	for dec.More() {
		if len(out) >= maxValues {
			return nil, tooMany
		}
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		s, ok := tok.(string)
		if !ok {
			return nil, errors.New("expected a list of strings")
		}
		out = append(out, s)
	}
	if err := closeBounded(dec, ']'); err != nil {
		return nil, err
	}
	return out, nil
}

// openBounded reads the next token: null (isNull) or the opening delimiter.
func openBounded(dec *json.Decoder, open json.Delim) (isNull bool, err error) {
	tok, err := dec.Token()
	if err != nil {
		return false, err
	}
	if tok == nil {
		return true, nil
	}
	if d, ok := tok.(json.Delim); !ok || d != open {
		if open == '[' {
			return false, errors.New("expected a list")
		}
		return false, errors.New("expected an object")
	}
	return false, nil
}

// closeBounded reads the closing delimiter.
func closeBounded(dec *json.Decoder, closing json.Delim) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := tok.(json.Delim); !ok || d != closing {
		return errors.New("malformed JSON")
	}
	return nil
}

// objectKey reads an object key.
func objectKey(dec *json.Decoder) (string, error) {
	tok, err := dec.Token()
	if err != nil {
		return "", err
	}
	key, ok := tok.(string)
	if !ok {
		return "", errors.New("malformed JSON")
	}
	return key, nil
}
