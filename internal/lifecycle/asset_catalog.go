package lifecycle

import "github.com/SecondStack-AI/SecondBox/internal/assetcatalog"

type Asset = assetcatalog.Asset
type AssetCatalog = assetcatalog.AssetCatalog
type FileAssetCatalog = assetcatalog.FileAssetCatalog

var LoadFileAssetCatalog = assetcatalog.LoadFileAssetCatalog
