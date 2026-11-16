package cli

const (
	aliPanWebsiteSource = "alipan-web"
	mobilePanSource     = "mobilepan"
)

func (m mountRecord) aliPanWebsite() bool {
	return m.Driver == "alipan" && m.Connection == "website" || m.Driver == aliPanWebsiteSource && m.Connection == ""
}
