# Shared CodeQL SARIF classifier. Used by BOTH steps in
# .github/workflows/codeql.yml (the summary and the gate) via `jq -f`, so the
# thing that reports and the thing that blocks can never drift apart — and so it
# can be exercised against fixture SARIF locally with the exact file CI runs.
#
# Input: `jq -rs --slurpfile reg .github/codeql-accepted-findings.json -f this`
#        over one or more CodeQL SARIF files.
# Output: one JSON object, `{blocking: [...], accepted: [...], advisory: [...],
#         ruleMetaCount: N, total: N}`. The caller formats it.
#
# WHY THE RULE LOOKUP IS THE WAY IT IS — this is the subtle part, and getting it
# wrong makes the gate silently vacuous rather than loudly broken:
#
# CodeQL writes query metadata into `runs[].tool.extensions[].rules[]` (one
# extension per query pack), NOT into `runs[].tool.driver.rules[]`. The driver is
# the CodeQL CLI itself. A first cut of this filter read only driver.rules, found
# nothing, and therefore scored EVERY finding at security-severity 0 — including
# go/request-forgery, whose real value is 9.1. The gate passed with "0 blocking"
# on a tree holding 30 findings, which is exactly the green-but-vacuous outcome
# the workflow exists to rule out. Verified against the actual SARIF from run
# 32583247659.
#
# For the same reason, a result's SEVERITY LEVEL usually is not on the result at
# all: SARIF says an omitted `level` falls back to the rule's
# `defaultConfiguration.level`, and CodeQL relies on that. So the level is
# resolved from the rule too, with the result's own `level` winning when present.
#
# `ruleMetaCount` is returned so the caller can fail closed when results exist
# but no rule metadata resolved — i.e. when this lookup has broken again.

# Every rule object anywhere in the tool description, keyed by id.
( [ .[] | .runs[]?
    | ( [ .tool.driver.rules[]? ] + [ .tool.extensions[]?.rules[]? ] )[]
    | select(.id != null)
  ] ) as $ruleList
| ( reduce $ruleList[] as $r ({}; .[$r.id] = $r) ) as $rules
| ( reduce ($reg[0].accepted[]? | "\(.rule) \(.file)") as $k ({}; .[$k] = true) ) as $waived
| ( [ .[] | .runs[]? | .results[]?
      | . as $res
      | ( $rules[$res.ruleId] // {} ) as $rule
      | ( ($rule.properties["security-severity"]) // "" ) as $sevRaw
      | ( $sevRaw | tonumber? // 0 ) as $sev
      | ( ($sevRaw | tonumber? | type == "number") // false ) as $hasSev
      | ( ($res.level) // ($rule.defaultConfiguration.level) // "note" ) as $level
      | ( ($res.locations[0].physicalLocation.artifactLocation.uri) // "" ) as $file
      | ( ($res.locations[0].physicalLocation.region.startLine) // "?" ) as $line
      | { rule: $res.ruleId,
          file: $file,
          line: $line,
          level: $level,
          sev: $sev,
          # An in-source `// codeql[rule-id]` comment lands here.
          suppressed: ((($res.suppressions // []) | length) > 0),
          waived: ($waived | has("\($res.ruleId) \($file)")),
          hasSev: $hasSev,
          # HIGH BAND. security-severity is the dimension that carries severity
          # information; CodeQL's own High/Critical cut is 7.0, and that is what
          # GitHub's code-scanning merge protection bands on.
          #
          # `level` (i.e. @problem.severity) is NOT a severity signal for a
          # security query — almost every one of them is `error`, including
          # go/log-injection at security-severity 6.1. Banding on level as well
          # would put all 23 log-injection findings in the blocking tier and
          # reproduce the any-finding deadlock this replaced. So level is used
          # ONLY as the fallback for a rule that publishes no security-severity
          # at all (a non-security query), where it is the only signal there is.
          high: (if $hasSev then $sev >= 7.0
                 else ($level == "error" or $level == "warning") end) }
    ] ) as $all
| { total: ($all | length),
    ruleMetaCount: ($ruleList | length),
    blocking: [ $all[] | select(.high and (.waived | not) and (.suppressed | not)) ],
    accepted: [ $all[] | select(.high and (.waived or .suppressed)) ],
    advisory: [ $all[] | select(.high | not) ] }
