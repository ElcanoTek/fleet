package agentcore

import (
	"os"
	"testing"
)

// openxExecuteDealTool is the OpenX MCP tool that executes a prepared deal from
// prompt-input parameters. Kept here (test-only) because several tests assert on
// it verbatim and it is part of the DSP fixture policy. Fleet ships no such tool
// in non-test code — the value lives only in the client bundle's manifest.
const openxExecuteDealTool = "mcp_openx_mcp_ox_execute_deal_from_prompt_inputs"

// TestMain installs the DSP fixture AgentPolicy for the whole agentcore test
// package before any test runs. The policy is configurable in production (loaded
// from the client bundle); the tests exercise the Elcano/ad-tech-DSP behavior,
// so they supply those tool names here as a FIXTURE. None of these DSP names
// live in non-test fleet code.
func TestMain(m *testing.M) {
	ConfigureAgentPolicy(testFixturePolicy())
	os.Exit(m.Run())
}

// testFixturePolicy is the full DSP policy the existing agentcore tests rely on:
// the verbatim port of the former parallelSafeMCPTools map, criticalToolSuffixes
// slice, and criticalToolSubstitutes map. Base critical suffixes (send_email,
// send_template_email) are merged in automatically by ConfigureAgentPolicy.
func testFixturePolicy() AgentPolicy {
	return AgentPolicy{
		ParallelSafeTools: []string{
			// Email + sendgrid
			"mcp_email_search_emails",
			"mcp_email_search_emails_fuzzy",
			"mcp_email_get_email",
			"mcp_email_download_attachment",
			"mcp_email_download_link_attachment",
			"mcp_email_extract_download_links",
			"mcp_email_find_latest_report",
			"mcp_sendgrid_validate_email_content",

			// Index Exchange — reads + per-deal executor
			"mcp_indexexchange_mcp_ix_auth_status",
			"mcp_indexexchange_mcp_ix_list_dsps",
			"mcp_indexexchange_mcp_ix_list_dsp_seats",
			"mcp_indexexchange_mcp_ix_list_deals_v3",
			"mcp_indexexchange_mcp_ix_get_deal_settings",
			"mcp_indexexchange_mcp_ix_list_marketplace_publishers",
			"mcp_indexexchange_mcp_ix_list_segments",
			"mcp_indexexchange_mcp_ix_list_targeting_keys",
			"mcp_indexexchange_mcp_ix_list_targeting_values",
			"mcp_indexexchange_mcp_ix_list_account_information",
			"mcp_indexexchange_mcp_ix_list_marketplace_report_presets",
			"mcp_indexexchange_mcp_ix_reporting_healthcheck",
			"mcp_indexexchange_mcp_ix_list_active_reports",
			"mcp_indexexchange_mcp_ix_list_report_files",
			"mcp_indexexchange_mcp_ix_execute_deal_from_prompt_inputs",

			// OpenX — reads + per-deal executor
			"mcp_openx_mcp_ox_list_demand_partners",
			"mcp_openx_mcp_ox_list_options_by_path",
			"mcp_openx_mcp_ox_list_iab_categories",
			"mcp_openx_mcp_ox_translate_iab_categories",
			"mcp_openx_mcp_ox_validate_audience_geo_compatibility",
			"mcp_openx_mcp_ox_list_fee_partners",
			"mcp_openx_mcp_ox_list_audience_segments",
			"mcp_openx_mcp_ox_list_states",
			"mcp_openx_mcp_ox_list_deals",
			"mcp_openx_mcp_ox_get_deal",
			"mcp_openx_mcp_ox_validate_domains",
			"mcp_openx_mcp_ox_introspect_type",
			"mcp_openx_mcp_ox_prepare_deal_from_brief",
			"mcp_openx_mcp_ox_prepare_deal_from_prompt_inputs",
			openxExecuteDealTool,

			// PubMatic — reads + per-deal executor
			"mcp_pubmatic_mcp_pm_auth_status",
			"mcp_pubmatic_mcp_pm_list_reporting_presets",
			"mcp_pubmatic_mcp_pm_reporting_healthcheck",
			"mcp_pubmatic_mcp_pm_get_curated_deal",
			"mcp_pubmatic_mcp_pm_prepare_deal_from_prompt_inputs",
			"mcp_pubmatic_mcp_pm_execute_deal_from_prompt_inputs",

			// Magnite — reads + per-deal executor
			"mcp_magnite_mcp_magnite_auth_status",
			"mcp_magnite_mcp_magnite_check_report_status",
			"mcp_magnite_mcp_magnite_list_reports",
			"mcp_magnite_mcp_magnite_get_deal",
			"mcp_magnite_mcp_magnite_list_marketplaces",
			"mcp_magnite_mcp_magnite_list_dsps",
			"mcp_magnite_mcp_magnite_list_dsp_buyers",
			"mcp_magnite_mcp_magnite_list_publishers",
			"mcp_magnite_mcp_magnite_list_audience_segments",
			"mcp_magnite_mcp_magnite_list_geo_values",
			"mcp_magnite_mcp_magnite_list_ad_formats",
			"mcp_magnite_mcp_magnite_list_targeting_lists",
			"mcp_magnite_mcp_magnite_get_rtd_signal",
			"mcp_magnite_mcp_magnite_list_rtd_signals",
			"mcp_magnite_mcp_magnite_prepare_deal_from_prompt_inputs",
			"mcp_magnite_mcp_magnite_execute_deal_from_prompt_inputs",

			// Xandr — reads
			"mcp_xandr_mcp_list_xandr_deals",
			"mcp_xandr_mcp_get_xandr_deal",
			"mcp_xandr_mcp_xandr_auth_status",
			"mcp_xandr_mcp_xandr_list_reporting_presets",
			"mcp_xandr_mcp_xandr_reporting_healthcheck",

			// TripleLift — reads
			"mcp_triplelift_mcp_tl_auth_status",
			"mcp_triplelift_mcp_tl_get_deal",
			"mcp_triplelift_mcp_tl_list_deals",
			"mcp_triplelift_mcp_tl_list_buyers",
			"mcp_triplelift_mcp_tl_list_countries",
			"mcp_triplelift_mcp_tl_list_segments",
			"mcp_triplelift_mcp_tl_get_avails",

			// MediaNet — reads + per-deal executor
			"mcp_medianet_mcp_mn_list_deals",
			"mcp_medianet_mcp_mn_get_deal",
			"mcp_medianet_mcp_mn_auth_status",
			"mcp_medianet_mcp_mn_prepare_deal_from_prompt_inputs",
			"mcp_medianet_mcp_mn_execute_deal_from_prompt_inputs",
			"mcp_medianet_mcp_mn_reporting_healthcheck",
			"mcp_medianet_mcp_mn_list_report_views",
			"mcp_medianet_mcp_mn_get_report_view_info",
			"mcp_medianet_mcp_mn_get_report_dimensions",
			"mcp_medianet_mcp_mn_get_report_metrics",
			"mcp_medianet_mcp_mn_get_report_relations",
		},
		// Non-base critical suffixes (the base send_email/send_template_email are
		// merged in by ConfigureAgentPolicy).
		CriticalToolSuffixes: []string{
			"generate_presentation",
			"generate_wrap_up_presentation",
			"generate_standard_presentation",
			"generate_and_wait_for_presentation",
			"generate_and_wait_for_wrap_up_presentation",
			"generate_and_wait_for_standard_presentation",
			"execute_deal_from_prompt_inputs",
			"create_marketplace_deal",
			"create_curated_deal",
			"create_xandr_deal",
			"create_prepared_deal",
			"create_deal",
		},
		CriticalToolSubstitutes: map[string][]string{
			"execute_deal_from_prompt_inputs": {
				"create_marketplace_deal",
				"create_curated_deal",
				"create_prepared_deal",
				"create_xandr_deal",
				"create_deal",
			},
		},
	}
}
