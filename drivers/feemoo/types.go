package feemoo

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/model"
)

// The web API uses both JSON numbers and decimal strings, including for IDs.
type scalar string

func (s *scalar) UnmarshalJSON(b []byte) error {
	if bytes.Equal(b, []byte("null")) {
		*s = ""
		return nil
	}
	var value string
	if len(b) > 0 && b[0] == '"' {
		if err := json.Unmarshal(b, &value); err != nil {
			return err
		}
	} else {
		var n json.Number
		if err := json.Unmarshal(b, &n); err != nil {
			return err
		}
		value = n.String()
	}
	*s = scalar(value)
	return nil
}

func (s scalar) int64() (int64, error) { return strconv.ParseInt(string(s), 10, 64) }

type envelope struct {
	Status scalar          `json:"status"`
	Msg    string          `json:"msg"`
	Data   json.RawMessage `json:"data"`
}

type userInfo struct {
	UploadType scalar `json:"upload_type"`
	PartSize   scalar `json:"upload_part_size"`
	MaxSize    scalar `json:"upload_max_limit"`
	UploadLine scalar `json:"upload_line"`
	MaxPart    scalar `json:"direct_upload_max_part"`
}

type entry struct {
	ID        scalar `json:"id"`
	Name      string `json:"name"`
	Extension string `json:"extension"`
	IsFile    scalar `json:"is_file"`
	Size      scalar `json:"orgin_size"` // Deliberate spelling in the real API.
	Time      string `json:"intime"`
}

func (e entry) object() (model.Obj, error) {
	if e.ID == "" || (e.IsFile != "0" && e.IsFile != "1") {
		return nil, fmt.Errorf("feemoo: invalid file-list entry")
	}
	name := e.Name
	size := int64(0)
	if e.IsFile == "1" {
		name += e.Extension // The list separates the basename and extension.
		var err error
		size, err = e.Size.int64()
		if err != nil || size < 0 {
			return nil, fmt.Errorf("feemoo: invalid file size in listing")
		}
	}
	if err := validateName(name); err != nil {
		return nil, err
	}
	location := time.FixedZone("Feemoo", 8*60*60)
	var modified time.Time
	for _, layout := range []string{"2006-01-02  15:04", "2006-01-02 15:04:05", "2006-01-02 15:04"} {
		var err error
		modified, err = time.ParseInLocation(layout, e.Time, location)
		if err == nil {
			break
		}
	}
	return &model.Object{ID: string(e.ID), Name: name, Size: size, IsFolder: e.IsFile == "0", Modified: modified}, nil
}

type listResult struct {
	List  []entry `json:"list"`
	Total scalar  `json:"total"`
}

type uploadSession struct {
	FileID    scalar `json:"file_id"`
	TaskID    scalar `json:"task_id"`
	PartURL   string `json:"upload_part_url"`
	MergeURL  string `json:"merge_part_url"`
	CancelURL string `json:"cancel_url"`
}
