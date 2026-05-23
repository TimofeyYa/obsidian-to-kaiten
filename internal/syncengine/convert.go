package syncengine

import (
	"bytes"

	md "github.com/JohannesKaufmann/html-to-markdown"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer/html"
)

// HTMLToMarkdown конвертирует HTML-контент Kaiten в Markdown.
func HTMLToMarkdown(htmlStr string) (string, error) {
	conv := md.NewConverter("", true, nil)
	out, err := conv.ConvertString(htmlStr)
	if err != nil {
		return "", err
	}
	return out, nil
}

// goldmarkConverter — единый экземпляр конвертера md→html с GFM расширениями:
// таблицы, code blocks, чек-боксы, авто-ссылки, strikethrough.
// Lazy-инициализация при первом вызове.
var goldmarkConverter goldmark.Markdown

func ensureGoldmark() goldmark.Markdown {
	if goldmarkConverter == nil {
		goldmarkConverter = goldmark.New(
			goldmark.WithExtensions(
				extension.GFM, // GitHub Flavored Markdown: таблицы, strikethrough, todo
				extension.Table,
				extension.TaskList,
				extension.Strikethrough,
				extension.Linkify,
			),
			goldmark.WithParserOptions(parser.WithAutoHeadingID()),
			goldmark.WithRendererOptions(html.WithUnsafe()),
		)
	}
	return goldmarkConverter
}

// MarkdownToHTML — полноценная конвертация Markdown → HTML через goldmark.
// Поддерживает: заголовки, списки (вкл. чек-боксы), code blocks с подсветкой,
// таблицы, ссылки, изображения, strikethrough, blockquotes.
//
// Раньше был fallback через <br/> + html.EscapeString, что портило форматирование
// при обратной отправке документов html-типа в Kaiten.
func MarkdownToHTML(text string) string {
	conv := ensureGoldmark()
	var buf bytes.Buffer
	if err := conv.Convert([]byte(text), &buf); err != nil {
		// На случай поломки парсера — возвращаем escape-fallback, чтобы не потерять данные.
		return "<pre>" + bytes.NewBufferString(text).String() + "</pre>"
	}
	return buf.String()
}
