use crate::settings::FontSettings;
use iced::{Font, Size};
use iced_core::{
    alignment::Vertical,
    text::{Alignment, LineHeight, Paragraph, Shaping as TextShaping},
    Text,
};
use iced_graphics::text::paragraph;

#[derive(Debug, Clone)]
pub struct TermFont {
    pub(crate) size: f32,
    pub(crate) font_type: Font,
    pub(crate) scale_factor: f32,
    pub(crate) measure: Size<f32>,
}

impl TermFont {
    pub fn new(settings: FontSettings) -> Self {
        Self {
            size: settings.size,
            font_type: settings.font_type,
            scale_factor: settings.scale_factor,
            measure: font_measure(
                settings.size,
                settings.scale_factor,
                settings.font_type,
            ),
        }
    }

    pub fn sync(&mut self) {
        self.measure =
            font_measure(self.size, self.scale_factor, self.font_type)
    }
}

fn font_measure(
    font_size: f32,
    scale_factor: f32,
    font_type: Font,
) -> Size<f32> {
    let paragraph = paragraph::Paragraph::with_text(Text {
        content: "m",
        font: font_type,
        size: iced_core::Pixels(font_size),
        align_y: Vertical::Center,
        align_x: Alignment::Center,
        shaping: TextShaping::Advanced,
        line_height: LineHeight::Relative(scale_factor),
        bounds: Size::INFINITE,
        wrapping: iced_core::text::Wrapping::Glyph,
    });

    // The backend and PTY use whole-pixel cells. Rounding down squeezes glyphs
    // into narrower cells and reports more columns than the font can fit.
    let bounds = paragraph.min_bounds();
    Size::new(bounds.width.ceil().max(1.0), bounds.height.ceil().max(1.0))
}

#[cfg(test)]
mod amux_tests {
    use super::*;

    #[test]
    fn fractional_font_sizes_use_whole_cells_without_squeezing_glyphs() {
        for size in [11.0, 13.5, 14.0, 17.25] {
            let font = TermFont::new(FontSettings {
                size,
                ..Default::default()
            });
            let glyph = paragraph::Paragraph::with_text(Text {
                content: "m",
                font: Font::MONOSPACE,
                size: iced_core::Pixels(size),
                bounds: Size::INFINITE,
                align_y: Vertical::Center,
                align_x: Alignment::Center,
                shaping: TextShaping::Advanced,
                line_height: LineHeight::Relative(1.3),
                wrapping: iced_core::text::Wrapping::Glyph,
            })
            .min_bounds();
            assert!(font.measure.width >= glyph.width);
            assert_eq!(font.measure.width.fract(), 0.0);
            assert_eq!(font.measure.height.fract(), 0.0);
        }
    }
}
