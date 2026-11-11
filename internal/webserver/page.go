package webserver

import (
	"github.com/gin-gonic/gin"
	"github.com/pfisterer/cloud-self-service-golib/ginweb"
	"github.com/pfisterer/cloud-self-service-golib/page"
)

// Every list a person or a script reads here is paged (golib page): ?limit (default 100, at most 500) and ?offset, answered as {items, total, limit, offset}, the shape of openstack-management-api's lists. The machine routes keep their whole lists: /v1/tree/projects is the sync of openstack-management-api, which deletes what it does not find there, and the fleet's /fleet/peers and /fleet/sites are read by the scripts on the machines. Views that are one picture of a bounded configuration (the fleet, the GPU administration, the model catalogue and the aliases) and the usage reports are no lists in this sense and stay whole.

// Page is page.Page as the API description shows it: swag reads no generic type from another module, so a paged route documents its answer as Page{items=[]T}, the same JSON (TestPageDocMatchesGolib).
type Page struct {
	Items any `json:"items" swaggerignore:"true"`
	// How many entries match, without paging.
	Total int `json:"total" binding:"required" example:"240"`
	// The page size that was applied.
	Limit int `json:"limit" binding:"required" example:"100"`
	// How many entries were skipped.
	Offset int `json:"offset" binding:"required" example:"0"`
}

// pageRequest reads ?limit and ?offset; bounds that do not parse are answered with 400 and ok is false.
func pageRequest(c *gin.Context) (page.Request, bool) { return ginweb.PageRequest(c) }
