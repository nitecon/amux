package desktop

import (
	"image/color"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/theme"
)

// WorkspaceTheme styles the native window and its snapshot fixtures.
func WorkspaceTheme() fyne.Theme {
	return workspaceTheme{Theme: theme.DarkTheme()}
}

type workspaceTheme struct {
	fyne.Theme
}

func (t workspaceTheme) Color(name fyne.ThemeColorName, variant fyne.ThemeVariant) color.Color {
	switch name {
	case theme.ColorNameBackground, theme.ColorNameForegroundOnPrimary:
		return color.NRGBA{R: 11, G: 15, B: 23, A: 255}
	case theme.ColorNameInputBackground:
		return color.NRGBA{R: 16, G: 22, B: 33, A: 255}
	case theme.ColorNameButton, theme.ColorNameMenuBackground:
		return color.NRGBA{R: 18, G: 26, B: 39, A: 255}
	case theme.ColorNameInputBorder, theme.ColorNameSeparator, theme.ColorNameHover:
		return color.NRGBA{R: 30, G: 41, B: 59, A: 255}
	case theme.ColorNameScrollBar:
		return color.NRGBA{R: 61, G: 82, B: 112, A: 255}
	case theme.ColorNameScrollBarBackground:
		return color.NRGBA{R: 20, G: 29, B: 46, A: 255}
	case theme.ColorNamePlaceHolder:
		return color.NRGBA{R: 146, G: 161, B: 185, A: 255}
	case theme.ColorNameDisabled:
		return color.NRGBA{R: 108, G: 123, B: 146, A: 255}
	case theme.ColorNameForeground:
		return color.NRGBA{R: 203, G: 213, B: 225, A: 255}
	case theme.ColorNamePrimary:
		return color.NRGBA{R: 34, G: 211, B: 238, A: 255}
	case theme.ColorNameFocus, theme.ColorNameSelection:
		return color.NRGBA{R: 8, G: 51, B: 68, A: 255}
	default:
		return t.Theme.Color(name, variant)
	}
}

func (t workspaceTheme) Size(name fyne.ThemeSizeName) float32 {
	switch name {
	case theme.SizeNameInnerPadding:
		return 4
	case theme.SizeNameButtonRadius:
		return 5
	case theme.SizeNameScrollBar:
		return 7
	case theme.SizeNameScrollBarSmall:
		return 4
	case theme.SizeNamePadding:
		return 6
	case theme.SizeNameText:
		return 13
	default:
		return t.Theme.Size(name)
	}
}

// toolbarTheme keeps navigation and filtering compact while preserving the
// regular body and heading styles in task details and conversations.
type toolbarTheme struct {
	fyne.Theme
}

func (t toolbarTheme) Font(style fyne.TextStyle) fyne.Resource {
	style.Bold = false
	return t.Theme.Font(style)
}

func (t toolbarTheme) Color(name fyne.ThemeColorName, variant fyne.ThemeVariant) color.Color {
	switch name {
	case theme.ColorNamePrimary:
		return t.Theme.Color(theme.ColorNameSelection, variant)
	case theme.ColorNameForegroundOnPrimary:
		return t.Theme.Color(theme.ColorNamePrimary, variant)
	default:
		return t.Theme.Color(name, variant)
	}
}

// cardListTheme lets the cards draw their own selected outline and fill.
type cardListTheme struct {
	fyne.Theme
}

func (t cardListTheme) Color(name fyne.ThemeColorName, variant fyne.ThemeVariant) color.Color {
	if name == theme.ColorNameSelection {
		return color.Transparent
	}
	return t.Theme.Color(name, variant)
}
