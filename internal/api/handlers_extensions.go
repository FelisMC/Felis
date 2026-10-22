package api

import (
	"net/http"

	"felis.lolicon.best/internal/fileedit"
)

func (a *API) handleExtensions(w http.ResponseWriter, r *http.Request) {
	name, ok := a.authorizeFileOp(w, r)
	if !ok {
		return
	}
	list, err := a.Files.Extensions(r.Context(), name)
	if err != nil {
		writeFileEditError(w, r, err)
		return
	}
	if list.Entries == nil {
		list.Entries = []fileedit.Extension{}
	}
	for i := range list.Entries {
		entry := &list.Entries[i]
		if entry.Metadata == nil {
			entry.Metadata = []fileedit.ExtensionMetadata{}
		}
		for j := range entry.Metadata {
			metadata := &entry.Metadata[j]
			if metadata.Authors == nil {
				metadata.Authors = []string{}
			}
			if metadata.Loaders == nil {
				metadata.Loaders = []string{}
			}
		}
	}
	writeJSON(w, http.StatusOK, list)
}
