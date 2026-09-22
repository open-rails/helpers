package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/open-rails/helpers/api"
)

func TestListWire(t *testing.T) {
	_, _, body := serve(t, func(w http.ResponseWriter, r *http.Request) {
		api.WriteJSON(w, http.StatusOK, api.NewList([]string{"a", "b"}, 10, 2, 0))
	})
	want := `{"object":"list","data":["a","b"],"total":10,"limit":2,"offset":0,"has_more":true}` + "\n"
	if body != want {
		t.Errorf("got %s want %s", body, want)
	}
}

func TestNewListNilDataIsEmptyArray(t *testing.T) {
	b, err := json.Marshal(api.NewList[string](nil, 0, 20, 0))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"object":"list","data":[],"total":0,"limit":20,"offset":0,"has_more":false}`
	if string(b) != want {
		t.Errorf("got %s want %s", b, want)
	}
}

func TestNewListHasMoreUsesRowsRead(t *testing.T) {
	// A short last page: 3 rows read at offset 8 of 11 exhausts the collection
	// even though offset+limit would say otherwise.
	if got := api.NewList([]int{1, 2, 3}, 11, 20, 8); got.HasMore {
		t.Error("has_more must be false when the rows read reach the total")
	}
	if got := api.NewList([]int{1, 2, 3}, 12, 20, 8); !got.HasMore {
		t.Error("has_more must be true when rows remain")
	}
}

func TestDeletedAndMessageWire(t *testing.T) {
	b, _ := json.Marshal(api.NewDeleted("gallery", "gal_123"))
	if want := `{"object":"gallery","id":"gal_123","deleted":true}`; string(b) != want {
		t.Errorf("got %s want %s", b, want)
	}
	b, _ = json.Marshal(api.NewMessage("saved"))
	if want := `{"object":"message","message":"saved"}`; string(b) != want {
		t.Errorf("got %s want %s", b, want)
	}
}
