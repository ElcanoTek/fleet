package httpapi

import "mime"

// Go's built-in mime.TypeByExtension only knows a handful of web-native
// extensions (html, png, svg, etc.). Office Open XML types — .pptx,
// .xlsx, .docx — fall through to whatever the host /etc/mime.types
// happens to carry, which is "not registered" on minimal containers
// and breaks the workspace file endpoint: ServeContent then sniffs the
// file as application/zip (PPTX is a ZIP under the hood) and browsers
// either save it as `.zip` or refuse the inline preview. Registering
// these explicitly here makes the deck/spreadsheet/doc files produced
// by the Gamma + run_python tooling serve with the correct MIME on
// every host, regardless of distro packaging.
//
// The values are the canonical OOXML media types from RFC 6713 / ISO
// 29500. Same as what /etc/mime.types ships on Fedora and Debian, just
// not reliant on the file being present.
func init() {
	for ext, ctype := range map[string]string{
		".pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
		".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
		".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		// Legacy binary office formats. Less common but the agent can
		// still produce them via python-pptx/openpyxl or LibreOffice
		// conversions, and the cost of registering is zero.
		".ppt": "application/vnd.ms-powerpoint",
		".xls": "application/vnd.ms-excel",
		".doc": "application/msword",
	} {
		// AddExtensionType only registers when the extension is not
		// already mapped, so a host /etc/mime.types that does include
		// these wins. (mime.AddExtensionType actually overwrites, but
		// the canonical OOXML strings match what the system files
		// carry, so there's no observable conflict.)
		_ = mime.AddExtensionType(ext, ctype)
	}
}
