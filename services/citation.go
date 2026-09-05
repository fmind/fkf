package services

import "strings"

func qualifiedCitation(baseName, uri string) string {
	if baseName == "" || uri == "" || strings.HasPrefix(uri, "fkf://") {
		return uri
	}
	return "fkf://" + baseName + "/" + uri
}
