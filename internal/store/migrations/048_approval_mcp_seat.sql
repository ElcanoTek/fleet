-- 048_approval_mcp_seat.sql — preserve the staged credential seat across an
-- approval card's lifetime (#167 residual 2).
--
-- An approval card can outlive the turn scope that staged it, so approval
-- execution used to fall back to the broker's default bundle seat. That is a
-- multi-account footgun: a turn that ran on a named account could have its
-- approved send executed as a different one.
--
-- mcp_server  : the bundle server name that authored the staged tool call.
--               NULL/empty for native tools (bash, preview_email) and for
--               legacy rows staged before this column existed.
-- mcp_account : the public named-account seat active when the call was staged.
--               Empty string means the default bundle seat was explicitly in
--               use; NULL means "not recorded" (legacy row), which execution
--               treats as the default seat exactly as it did before.
ALTER TABLE approvals ADD COLUMN mcp_server TEXT;
ALTER TABLE approvals ADD COLUMN mcp_account TEXT;
