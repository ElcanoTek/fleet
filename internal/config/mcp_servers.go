package config

// Scheduled-mode credential-gated MCP server catalog (lifted from cutlass
// config.go). configureMCPServers builds c.MCPServers from the loaded
// credentials; a server is included only when its isEnabled gate passes. The
// per-server env builders produce the BASE (default-seat) env — per-account
// <VAR>_<ACCOUNT> overlays are applied at bind time via
// creds.ApplyClientSuffix, not here.

// mcpServerDefinition defines a potential MCP server configuration.
type mcpServerDefinition struct {
	name          string
	serverType    string
	command       string
	args          []string
	URL           string
	envBuilder    func(c *Config) map[string]string
	headerBuilder func(c *Config) map[string]string
	isEnabled     func(c *Config) bool
	toolAllowlist []string
}

func buildTripleLiftEnv(c *Config) map[string]string {
	env := map[string]string{
		"TRIPLELIFT_CLIENT_ID":     c.TripleLiftClientID,
		"TRIPLELIFT_CLIENT_SECRET": c.TripleLiftClientSecret,
	}
	if c.TripleLiftMemberID != "" {
		env["TRIPLELIFT_MEMBER_ID"] = c.TripleLiftMemberID
	}
	if c.TripleLiftTokenURL != "" {
		env["TRIPLELIFT_TOKEN_URL"] = c.TripleLiftTokenURL
	}
	if c.TripleLiftBaseURL != "" {
		env["TRIPLELIFT_BASE_URL"] = c.TripleLiftBaseURL
	}
	if c.TripleLiftAudience != "" {
		env["TRIPLELIFT_AUDIENCE"] = c.TripleLiftAudience
	}
	if c.TripleLiftOrganization != "" {
		env["TRIPLELIFT_ORGANIZATION"] = c.TripleLiftOrganization
	}
	if c.TripleLiftScope != "" {
		env["TRIPLELIFT_SCOPE"] = c.TripleLiftScope
	}
	if c.TripleLiftReportingBaseURL != "" {
		env["TRIPLELIFT_REPORTING_BASE_URL"] = c.TripleLiftReportingBaseURL
	}
	if c.TripleLiftReportDownloadDir != "" {
		env["TRIPLELIFT_REPORT_DOWNLOAD_DIR"] = c.TripleLiftReportDownloadDir
	}
	return env
}

// getMCPServerDefinitions returns all possible MCP server configurations.
func getMCPServerDefinitions() []mcpServerDefinition {
	return []mcpServerDefinition{
		{
			name:       "deal_sheet",
			serverType: mcpServerTypeStdio,
			command:    mcpCommandPython3,
			args:       []string{"mcp/deal_sheet_server.py"},
			isEnabled:  func(_ *Config) bool { return true },
			envBuilder: func(c *Config) map[string]string {
				env := map[string]string{}
				if c.DealSheetOutputDir != "" {
					env["DEAL_SHEET_OUTPUT_DIR"] = c.DealSheetOutputDir
				}
				return env
			},
		},
		{
			name:       "sendgrid",
			serverType: mcpServerTypeStdio,
			command:    mcpCommandPython3,
			args:       []string{"mcp/sendgrid_server.py"},
			isEnabled:  func(c *Config) bool { return c.SendGridAPIKey != "" },
			envBuilder: func(c *Config) map[string]string {
				return map[string]string{
					envSendGridAPIKey:    c.SendGridAPIKey,
					envSendGridFromEmail: c.SendGridFromEmail,
				}
			},
		},
		{
			name:       "email",
			serverType: mcpServerTypeStdio,
			command:    mcpCommandPython3,
			args:       []string{"mcp/ses_s3_email.py"},
			isEnabled:  func(c *Config) bool { return c.EmailS3Bucket != "" },
			envBuilder: func(c *Config) map[string]string {
				env := map[string]string{
					envEmailS3Bucket: c.EmailS3Bucket,
					envEmailS3Prefix: c.EmailS3Prefix,
				}
				if c.AWSAccessKeyID != "" {
					env["AWS_ACCESS_KEY_ID"] = c.AWSAccessKeyID
				}
				if c.AWSSecretAccessKey != "" {
					env["AWS_SECRET_ACCESS_KEY"] = c.AWSSecretAccessKey
				}
				if c.AWSRegion != "" {
					env["AWS_REGION"] = c.AWSRegion
				}
				if c.EmailS3DatePrefixFormat != "" {
					env["EMAIL_S3_DATE_PREFIX_FORMAT"] = c.EmailS3DatePrefixFormat
				}
				if c.EmailS3MaxDatePrefixDays != "" {
					env["EMAIL_S3_MAX_DATE_PREFIX_DAYS"] = c.EmailS3MaxDatePrefixDays
				}
				if c.EmailAttachmentDir != "" {
					env["EMAIL_ATTACHMENT_DIR"] = c.EmailAttachmentDir
				}
				if c.EmailLastCheckFile != "" {
					env["EMAIL_LAST_CHECK_FILE"] = c.EmailLastCheckFile
				}
				return env
			},
		},
		{
			name:       "openx_mcp",
			serverType: mcpServerTypeStdio,
			command:    mcpCommandPython3,
			args:       []string{"mcp/openx_mcp.py"},
			isEnabled:  func(c *Config) bool { return c.OpenXAPIKey != "" },
			envBuilder: func(c *Config) map[string]string {
				return map[string]string{
					envOpenXAPIKey: c.OpenXAPIKey,
				}
			},
		},
		{
			name:       "pubmatic_mcp",
			serverType: mcpServerTypeStdio,
			command:    mcpCommandPython3,
			args:       []string{"mcp/pubmatic_mcp.py"},
			isEnabled: func(c *Config) bool {
				return c.PubMaticAccessToken != "" || (c.PubMaticUsername != "" && c.PubMaticPassword != "")
			},
			envBuilder: func(c *Config) map[string]string {
				return map[string]string{
					"PUBMATIC_API_KEY":             c.PubMaticAPIKey,
					"PUBMATIC_BASE_URL":            c.PubMaticMCPBaseURL,
					"PUBMATIC_MCP_BASE_URL":        c.PubMaticMCPBaseURL,
					"PUBMATIC_USERNAME":            c.PubMaticUsername,
					"PUBMATIC_PASSWORD":            c.PubMaticPassword,
					"PUBMATIC_API_PRODUCT":         c.PubMaticAPIProduct,
					"PUBMATIC_ACCESS_TOKEN":        c.PubMaticAccessToken,
					"PUBMATIC_DSP_ID":              c.PubMaticDSPID,
					"PUBMATIC_BUYER_ID":            c.PubMaticBuyerID,
					"PUBMATIC_SEAT_ID":             c.PubMaticSeatID,
					"PUBMATIC_TARGETING_ID":        c.PubMaticTargetingID,
					envPubMaticOwnerID:             c.PubMaticOwnerID,
					"PUBMATIC_REPORT_DOWNLOAD_DIR": c.PubMaticReportDownloadDir,
				}
			},
		},
		{
			name:       "medianet_mcp",
			serverType: mcpServerTypeStdio,
			command:    mcpCommandPython3,
			args:       []string{"mcp/medianet_mcp.py"},
			isEnabled: func(c *Config) bool {
				return c.MediaNetSelectToken != "" || (c.MediaNetSelectEmail != "" && c.MediaNetSelectPassword != "")
			},
			envBuilder: func(c *Config) map[string]string {
				return map[string]string{
					"MEDIANET_SELECT_BASE_URL":     c.MediaNetSelectBaseURL,
					"MEDIANET_SELECT_EMAIL":        c.MediaNetSelectEmail,
					"MEDIANET_SELECT_PASSWORD":     c.MediaNetSelectPassword,
					"MEDIANET_SELECT_TOKEN":        c.MediaNetSelectToken,
					"MEDIANET_REPORT_BASE_URL":     c.MediaNetReportBaseURL,
					"MEDIANET_REPORT_DOWNLOAD_DIR": c.MediaNetReportDownloadDir,
				}
			},
		},
		{
			name:       "magnite_mcp",
			serverType: mcpServerTypeStdio,
			command:    mcpCommandPython3,
			args:       []string{"mcp/magnite_mcp.py"},
			toolAllowlist: []string{
				"magnite_auth_status",
				"magnite_create_offline_report",
				"magnite_check_report_status",
				"magnite_list_reports",
				"magnite_download_report",
				"magnite_run_report_from_prompt_inputs",
				"magnite_create_deal",
				"magnite_get_deal",
				"magnite_update_deal",
				"magnite_activate_deal",
				"magnite_deactivate_deal",
				"magnite_list_marketplaces",
				"magnite_list_dsps",
				"magnite_list_dsp_buyers",
				"magnite_list_publishers",
				"magnite_list_audience_segments",
				"magnite_list_geo_values",
				"magnite_list_ad_formats",
				"magnite_list_targeting_lists",
				"magnite_create_rtd_signal",
				"magnite_get_rtd_signal",
				"magnite_update_rtd_signal",
				"magnite_list_rtd_signals",
				"magnite_prepare_deal_from_prompt_inputs",
				"magnite_create_prepared_deal",
				"magnite_execute_deal_from_prompt_inputs",
			},
			isEnabled: func(c *Config) bool { return c.MagniteAccessKey != "" && c.MagniteSecretKey != "" },
			envBuilder: func(c *Config) map[string]string {
				env := map[string]string{
					"MAGNITE_ACCESS_KEY": c.MagniteAccessKey,
					"MAGNITE_SECRET_KEY": c.MagniteSecretKey,
					"MAGNITE_SEAT_ID":    c.MagniteSeatID,
				}
				if c.MagniteAccountID != "" {
					env["MAGNITE_ACCOUNT_ID"] = c.MagniteAccountID
				}
				if c.MagniteDVBaseURL != "" {
					env["MAGNITE_DV_BASE_URL"] = c.MagniteDVBaseURL
				}
				if c.MagniteDMGBaseURL != "" {
					env["MAGNITE_DMG_BASE_URL"] = c.MagniteDMGBaseURL
				}
				if c.MagniteDownloadDir != "" {
					env["MAGNITE_DOWNLOAD_DIR"] = c.MagniteDownloadDir
				}
				return env
			},
		},
		{
			name:       "indexexchange_mcp",
			serverType: mcpServerTypeStdio,
			command:    mcpCommandPython3,
			args:       []string{"mcp/indexexchange_mcp.py"},
			isEnabled: func(c *Config) bool {
				return (c.IndexExchangeServiceID != "" && c.IndexExchangeServiceSecret != "") ||
					(c.IndexExchangeUsername != "" && c.IndexExchangePassword != "")
			},
			envBuilder: func(c *Config) map[string]string {
				env := map[string]string{
					envIndexExchangeBaseURL:              c.IndexExchangeBaseURL,
					envIndexExchangeMarketplaceAccountID: c.IndexExchangeMarketplaceAccountID,
				}
				if c.IndexExchangeServiceID != "" {
					env["INDEXEXCHANGE_SERVICE_ID"] = c.IndexExchangeServiceID
				}
				if c.IndexExchangeServiceSecret != "" {
					env["INDEXEXCHANGE_SERVICE_SECRET"] = c.IndexExchangeServiceSecret
				}
				if c.IndexExchangeUsername != "" {
					env["INDEXEXCHANGE_USERNAME"] = c.IndexExchangeUsername
				}
				if c.IndexExchangePassword != "" {
					env["INDEXEXCHANGE_PASSWORD"] = c.IndexExchangePassword
				}
				if c.IndexExchangeTimeoutSeconds != "" {
					env["INDEXEXCHANGE_TIMEOUT_SECONDS"] = c.IndexExchangeTimeoutSeconds
				}
				if c.IndexExchangeDownloadDir != "" {
					env["INDEXEXCHANGE_DOWNLOAD_DIR"] = c.IndexExchangeDownloadDir
				}
				return env
			},
		},
		{
			name:       "xandr_mcp",
			serverType: mcpServerTypeStdio,
			command:    mcpCommandPython3,
			args:       []string{"mcp/xandr_mcp.py"},
			isEnabled:  func(c *Config) bool { return c.XandrUsername != "" && c.XandrPassword != "" },
			envBuilder: func(c *Config) map[string]string {
				env := map[string]string{
					"XANDR_USERNAME":            c.XandrUsername,
					"XANDR_PASSWORD":            c.XandrPassword,
					"XANDR_REPORT_DOWNLOAD_DIR": c.XandrReportDownloadDir,
				}
				if c.XandrSeatID != "" {
					env["XANDR_SEAT_ID"] = c.XandrSeatID
				}
				return env
			},
		},
		{
			name:       "triplelift_mcp",
			serverType: mcpServerTypeStdio,
			command:    mcpCommandPython3,
			args:       []string{"mcp/triplelift_mcp.py"},
			isEnabled:  func(c *Config) bool { return c.TripleLiftClientID != "" && c.TripleLiftClientSecret != "" },
			envBuilder: buildTripleLiftEnv,
		},
		{
			name:       "fast_io",
			serverType: mcpServerTypeHTTP,
			URL:        "https://mcp.fast.io/mcp",
			isEnabled:  func(c *Config) bool { return c.FastIOMCPToken != "" },
			headerBuilder: func(c *Config) map[string]string {
				return map[string]string{
					"Authorization": "Bearer " + c.FastIOMCPToken,
				}
			},
			toolAllowlist: []string{
				"storage",
				"download",
				"upload",
				"workspace",
				"share",
				"worklog",
				"comment",
			},
		},
	}
}

func (c *Config) configureMCPServers() {
	for _, def := range getMCPServerDefinitions() {
		if !def.isEnabled(c) {
			continue
		}

		serverConfig := MCPServerConfig{
			Type:    def.serverType,
			Enabled: true,
		}

		switch def.serverType {
		case mcpServerTypeStdio:
			serverConfig.Command = def.command
			serverConfig.Args = def.args
			if def.envBuilder != nil {
				serverConfig.Env = def.envBuilder(c)
			}
		case mcpServerTypeHTTP:
			serverConfig.URL = def.URL
			if def.headerBuilder != nil {
				serverConfig.Headers = def.headerBuilder(c)
			}
		}

		serverConfig.ToolAllowlist = def.toolAllowlist
		c.MCPServers[def.name] = serverConfig
	}
}
