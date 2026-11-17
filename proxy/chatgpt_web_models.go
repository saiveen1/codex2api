package proxy

import "slices"

// Public compatibility names supported by the Web worker. The catalog is not
// an account readiness snapshot or a claim about the upstream engine version.
var chatGPTWebImageModels = []string{
	"gpt-image-2",
	"gpt-image-2.5",
	"gpt-image-2.5-flare",
	"gpt-image-2.5-sunburst",
	"gpt-image-2.6",
	"gpt-image-2.6-flare",
	"gpt-image-2.6-sunburst",
}

func isChatGPTWebImageModel(model string) bool {
	return slices.Contains(chatGPTWebImageModels, model)
}
