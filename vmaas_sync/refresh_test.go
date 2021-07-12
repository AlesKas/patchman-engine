package vmaas_sync //nolint:revive,stylecheck

import (
	"app/base/database"
	"github.com/stretchr/testify/assert"
	"testing"
)

type CachedPackage struct {
	NameID    int
	PackageID int
	Summary   string
}

func TestPackageLatestCacheSort(t *testing.T) {
	var packageLast CachedPackage
	database.Db.Table("package_latest_cache").Last(&packageLast)
	assert.Equal(t, packageLast.NameID, 111)
	assert.Equal(t, packageLast.PackageID, 8)
}
