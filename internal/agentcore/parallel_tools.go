package agentcore

// parallelSafeMCPTools lists MCP tools safe to dispatch concurrently within a
// single assistant turn (lifted verbatim from cutlass fantasy.go). Names are the
// prefixed fantasy tool names (mcp_<server>_<tool>). Conservative by design:
// any tool that writes shared/global state MUST NOT appear here.

// openxExecuteDealTool is the OpenX MCP tool that creates a prepared deal
// from prompt-input parameters. Hoisted to a constant because it is the
// canonical reference value the orchestration's critical-action machinery
// matches against and several tests assert on it verbatim.
const openxExecuteDealTool = "mcp_openx_mcp_ox_execute_deal_from_prompt_inputs"

var parallelSafeMCPTools = map[string]bool{
	// Email + sendgrid
	"mcp_email_search_emails":             true,
	"mcp_email_search_emails_fuzzy":       true,
	"mcp_email_get_email":                 true,
	"mcp_email_download_attachment":       true,
	"mcp_email_download_link_attachment":  true,
	"mcp_email_extract_download_links":    true,
	"mcp_email_find_latest_report":        true,
	"mcp_sendgrid_validate_email_content": true,

	// Index Exchange — reads + per-deal executor
	"mcp_indexexchange_mcp_ix_auth_status":                     true,
	"mcp_indexexchange_mcp_ix_list_dsps":                       true,
	"mcp_indexexchange_mcp_ix_list_dsp_seats":                  true,
	"mcp_indexexchange_mcp_ix_list_deals_v3":                   true,
	"mcp_indexexchange_mcp_ix_get_deal_settings":               true,
	"mcp_indexexchange_mcp_ix_list_marketplace_publishers":     true,
	"mcp_indexexchange_mcp_ix_list_segments":                   true,
	"mcp_indexexchange_mcp_ix_list_targeting_keys":             true,
	"mcp_indexexchange_mcp_ix_list_targeting_values":           true,
	"mcp_indexexchange_mcp_ix_list_account_information":        true,
	"mcp_indexexchange_mcp_ix_list_marketplace_report_presets": true,
	"mcp_indexexchange_mcp_ix_reporting_healthcheck":           true,
	"mcp_indexexchange_mcp_ix_list_active_reports":             true,
	"mcp_indexexchange_mcp_ix_list_report_files":               true,
	"mcp_indexexchange_mcp_ix_execute_deal_from_prompt_inputs": true,

	// OpenX — reads + per-deal executor
	"mcp_openx_mcp_ox_list_demand_partners":                true,
	"mcp_openx_mcp_ox_list_options_by_path":                true,
	"mcp_openx_mcp_ox_list_iab_categories":                 true,
	"mcp_openx_mcp_ox_translate_iab_categories":            true,
	"mcp_openx_mcp_ox_validate_audience_geo_compatibility": true,
	"mcp_openx_mcp_ox_list_fee_partners":                   true,
	"mcp_openx_mcp_ox_list_audience_segments":              true,
	"mcp_openx_mcp_ox_list_states":                         true,
	"mcp_openx_mcp_ox_list_deals":                          true,
	"mcp_openx_mcp_ox_get_deal":                            true,
	"mcp_openx_mcp_ox_validate_domains":                    true,
	"mcp_openx_mcp_ox_introspect_type":                     true,
	"mcp_openx_mcp_ox_prepare_deal_from_brief":             true,
	"mcp_openx_mcp_ox_prepare_deal_from_prompt_inputs":     true,
	openxExecuteDealTool:                                   true,

	// PubMatic — reads + per-deal executor
	"mcp_pubmatic_mcp_pm_auth_status":                     true,
	"mcp_pubmatic_mcp_pm_list_reporting_presets":          true,
	"mcp_pubmatic_mcp_pm_reporting_healthcheck":           true,
	"mcp_pubmatic_mcp_pm_get_curated_deal":                true,
	"mcp_pubmatic_mcp_pm_prepare_deal_from_prompt_inputs": true,
	"mcp_pubmatic_mcp_pm_execute_deal_from_prompt_inputs": true,

	// Magnite — reads + per-deal executor. DV+ reporting reads plus the
	// ClearLine Demand Management reference-data lookups and the
	// prepare/execute prompt-inputs pair (each execute call carries its own
	// deal payload, mirroring the other SSPs' executors). The low-level
	// magnite_create_deal / magnite_update_deal / activate / deactivate /
	// rtd-signal writes are intentionally NOT parallel-safe.
	"mcp_magnite_mcp_magnite_auth_status":                     true,
	"mcp_magnite_mcp_magnite_check_report_status":             true,
	"mcp_magnite_mcp_magnite_list_reports":                    true,
	"mcp_magnite_mcp_magnite_get_deal":                        true,
	"mcp_magnite_mcp_magnite_list_marketplaces":               true,
	"mcp_magnite_mcp_magnite_list_dsps":                       true,
	"mcp_magnite_mcp_magnite_list_dsp_buyers":                 true,
	"mcp_magnite_mcp_magnite_list_publishers":                 true,
	"mcp_magnite_mcp_magnite_list_audience_segments":          true,
	"mcp_magnite_mcp_magnite_list_geo_values":                 true,
	"mcp_magnite_mcp_magnite_list_ad_formats":                 true,
	"mcp_magnite_mcp_magnite_list_targeting_lists":            true,
	"mcp_magnite_mcp_magnite_get_rtd_signal":                  true,
	"mcp_magnite_mcp_magnite_list_rtd_signals":                true,
	"mcp_magnite_mcp_magnite_prepare_deal_from_prompt_inputs": true,
	"mcp_magnite_mcp_magnite_execute_deal_from_prompt_inputs": true,

	// Xandr — reads
	"mcp_xandr_mcp_list_xandr_deals":             true,
	"mcp_xandr_mcp_get_xandr_deal":               true,
	"mcp_xandr_mcp_xandr_auth_status":            true,
	"mcp_xandr_mcp_xandr_list_reporting_presets": true,
	"mcp_xandr_mcp_xandr_reporting_healthcheck":  true,

	// TripleLift — reads
	"mcp_triplelift_mcp_tl_auth_status":    true,
	"mcp_triplelift_mcp_tl_get_deal":       true,
	"mcp_triplelift_mcp_tl_list_deals":     true,
	"mcp_triplelift_mcp_tl_list_buyers":    true,
	"mcp_triplelift_mcp_tl_list_countries": true,
	"mcp_triplelift_mcp_tl_list_segments":  true,
	"mcp_triplelift_mcp_tl_get_avails":     true,

	// MediaNet — reads + per-deal executor
	"mcp_medianet_mcp_mn_list_deals":                      true,
	"mcp_medianet_mcp_mn_get_deal":                        true,
	"mcp_medianet_mcp_mn_auth_status":                     true,
	"mcp_medianet_mcp_mn_prepare_deal_from_prompt_inputs": true,
	"mcp_medianet_mcp_mn_execute_deal_from_prompt_inputs": true,
	"mcp_medianet_mcp_mn_reporting_healthcheck":           true,
	"mcp_medianet_mcp_mn_list_report_views":               true,
	"mcp_medianet_mcp_mn_get_report_view_info":            true,
	"mcp_medianet_mcp_mn_get_report_dimensions":           true,
	"mcp_medianet_mcp_mn_get_report_metrics":              true,
	"mcp_medianet_mcp_mn_get_report_relations":            true,
}
