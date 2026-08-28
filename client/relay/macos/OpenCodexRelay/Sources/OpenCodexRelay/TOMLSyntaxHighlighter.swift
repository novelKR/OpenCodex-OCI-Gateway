import AppKit
import Foundation
import SwiftUI

enum TOMLSyntaxTokenKind: Equatable, Sendable {
    case table
    case key
    case stringLiteral
    case numberOrDate
    case booleanLiteral
    case comment
}

struct TOMLSyntaxToken: Equatable, Sendable {
    let kind: TOMLSyntaxTokenKind
    let range: NSRange
}

enum TOMLSyntaxHighlighter {
    private enum MultilineDelimiter {
        case basic
        case literal

        var marker: String {
            switch self {
            case .basic: "\"\"\""
            case .literal: "'''"
            }
        }
    }

    private static let hash = unichar(35)
    private static let apostrophe = unichar(39)
    private static let quote = unichar(34)
    private static let equals = unichar(61)
    private static let openingBracket = unichar(91)

    static func highlighted(_ source: String) -> NSAttributedString {
        let baseFont = NSFont.monospacedSystemFont(
            ofSize: NSFont.systemFontSize,
            weight: .regular
        )
        let output = NSMutableAttributedString(
            string: source,
            attributes: [
                .font: baseFont,
                .foregroundColor: NSColor.labelColor,
            ]
        )
        for token in tokens(in: source) {
            output.addAttributes(
                attributes(for: token.kind),
                range: token.range
            )
        }
        return NSAttributedString(attributedString: output)
    }

    private static func attributes(
        for kind: TOMLSyntaxTokenKind
    ) -> [NSAttributedString.Key: Any] {
        let fontSize = NSFont.systemFontSize
        switch kind {
        case .table:
            return [
                .foregroundColor: NSColor.systemPurple,
                .font: NSFont.monospacedSystemFont(
                    ofSize: fontSize,
                    weight: .semibold
                ),
            ]
        case .key:
            return [
                .foregroundColor: NSColor.systemBlue,
                .font: NSFont.monospacedSystemFont(
                    ofSize: fontSize,
                    weight: .medium
                ),
            ]
        case .stringLiteral:
            return [.foregroundColor: NSColor.systemGreen]
        case .numberOrDate:
            return [.foregroundColor: NSColor.systemOrange]
        case .booleanLiteral:
            return [
                .foregroundColor: NSColor.systemPink,
                .font: NSFont.monospacedSystemFont(
                    ofSize: fontSize,
                    weight: .medium
                ),
            ]
        case .comment:
            return [
                .foregroundColor: NSColor.secondaryLabelColor,
                .obliqueness: 0.12,
            ]
        }
    }

    static func tokens(in source: String) -> [TOMLSyntaxToken] {
        let text = source as NSString
        var result: [TOMLSyntaxToken] = []
        var multiline: MultilineDelimiter?
        var lineStart = 0

        while lineStart < text.length {
            let remaining = NSRange(
                location: lineStart,
                length: text.length - lineStart
            )
            let newline = text.range(of: "\n", options: [], range: remaining)
            let lineEnd = newline.location == NSNotFound
                ? text.length
                : newline.location
            scanLine(
                text,
                range: NSRange(location: lineStart, length: lineEnd - lineStart),
                multiline: &multiline,
                into: &result
            )
            guard newline.location != NSNotFound else { break }
            lineStart = newline.location + newline.length
        }
        return result
    }

    private static func scanLine(
        _ text: NSString,
        range: NSRange,
        multiline: inout MultilineDelimiter?,
        into result: inout [TOMLSyntaxToken]
    ) {
        let lineEnd = NSMaxRange(range)
        var cursor = range.location

        if let activeDelimiter = multiline {
            if let closingEnd = multilineClosingEnd(
                in: text,
                from: cursor,
                to: lineEnd,
                delimiter: activeDelimiter
            ) {
                append(.stringLiteral, cursor..<closingEnd, to: &result)
                multiline = nil
                scanValue(
                    text,
                    from: closingEnd,
                    to: lineEnd,
                    multiline: &multiline,
                    into: &result
                )
                return
            } else {
                append(.stringLiteral, cursor..<lineEnd, to: &result)
                return
            }
        }

        cursor = skipWhitespace(in: text, from: cursor, to: lineEnd)
        guard cursor < lineEnd else { return }

        if text.character(at: cursor) == hash {
            append(.comment, cursor..<trimmedEnd(in: text, from: cursor, to: lineEnd), to: &result)
            return
        }

        if text.character(at: cursor) == openingBracket,
           isTableHeader(in: text, from: cursor, to: lineEnd) {
            let contentEnd = commentBoundary(in: text, from: cursor, to: lineEnd)
            append(.table, cursor..<trimmedEnd(in: text, from: cursor, to: contentEnd), to: &result)
            if contentEnd < lineEnd {
                append(.comment, contentEnd..<trimmedEnd(in: text, from: contentEnd, to: lineEnd), to: &result)
            }
            return
        }

        if text.character(at: cursor) != unichar(123),
           text.character(at: cursor) != openingBracket,
           let assignment = assignmentIndex(in: text, from: cursor, to: lineEnd) {
            append(.key, cursor..<trimmedEnd(in: text, from: cursor, to: assignment), to: &result)
            cursor = assignment + 1
        }
        scanValue(
            text,
            from: cursor,
            to: lineEnd,
            multiline: &multiline,
            into: &result
        )
    }

    private static func scanValue(
        _ text: NSString,
        from start: Int,
        to end: Int,
        multiline: inout MultilineDelimiter?,
        into result: inout [TOMLSyntaxToken]
    ) {
        var cursor = start
        while cursor < end {
            cursor = skipWhitespace(in: text, from: cursor, to: end)
            guard cursor < end else { return }

            let character = text.character(at: cursor)
            if character == hash {
                append(.comment, cursor..<trimmedEnd(in: text, from: cursor, to: end), to: &result)
                return
            }
            if character == quote || character == apostrophe {
                let isBasic = character == quote
                let delimiter: MultilineDelimiter = isBasic ? .basic : .literal
                if hasMarker(delimiter.marker, in: text, at: cursor, to: end) {
                    if let closingEnd = multilineClosingEnd(
                        in: text,
                        from: cursor + 3,
                        to: end,
                        delimiter: delimiter
                    ) {
                        let kind = isFollowedByAssignment(
                            in: text,
                            after: closingEnd,
                            to: end
                        ) ? TOMLSyntaxTokenKind.key : .stringLiteral
                        append(kind, cursor..<closingEnd, to: &result)
                        cursor = closingEnd
                    } else {
                        append(.stringLiteral, cursor..<end, to: &result)
                        multiline = delimiter
                        return
                    }
                } else {
                    let closingEnd = quotedClosingEnd(
                        in: text,
                        from: cursor + 1,
                        to: end,
                        quote: character,
                        honorsEscapes: isBasic
                    )
                    let kind = isFollowedByAssignment(
                        in: text,
                        after: closingEnd,
                        to: end
                    ) ? TOMLSyntaxTokenKind.key : .stringLiteral
                    append(kind, cursor..<closingEnd, to: &result)
                    cursor = closingEnd
                }
                continue
            }

            guard isAtomStart(character) else {
                cursor += 1
                continue
            }
            let atomStart = cursor
            while cursor < end, !isAtomDelimiter(text.character(at: cursor)) {
                cursor += 1
            }
            let atom = text.substring(with: NSRange(
                location: atomStart,
                length: cursor - atomStart
            ))
            let kind: TOMLSyntaxTokenKind?
            if isFollowedByAssignment(in: text, after: cursor, to: end) {
                kind = .key
            } else if atom == "true" || atom == "false" {
                kind = .booleanLiteral
            } else if looksLikeNumberOrDate(atom) {
                kind = .numberOrDate
            } else {
                kind = nil
            }
            if let kind {
                append(kind, atomStart..<cursor, to: &result)
            }
        }
    }

    private static func assignmentIndex(
        in text: NSString,
        from start: Int,
        to end: Int
    ) -> Int? {
        var cursor = start
        var activeQuote: unichar?
        var escaped = false
        while cursor < end {
            let character = text.character(at: cursor)
            if let quote = activeQuote {
                if quote == Self.quote, character == unichar(92), !escaped {
                    escaped = true
                } else {
                    if character == quote, !escaped {
                        activeQuote = nil
                    }
                    escaped = false
                }
            } else if character == quote || character == apostrophe {
                activeQuote = character
            } else if character == hash {
                return nil
            } else if character == equals {
                return cursor
            }
            cursor += 1
        }
        return nil
    }

    private static func commentBoundary(
        in text: NSString,
        from start: Int,
        to end: Int
    ) -> Int {
        var cursor = start
        var activeQuote: unichar?
        var escaped = false
        while cursor < end {
            let character = text.character(at: cursor)
            if let quote = activeQuote {
                if quote == Self.quote, character == unichar(92), !escaped {
                    escaped = true
                } else {
                    if character == quote, !escaped {
                        activeQuote = nil
                    }
                    escaped = false
                }
            } else if character == quote || character == apostrophe {
                activeQuote = character
            } else if character == hash {
                return cursor
            }
            cursor += 1
        }
        return end
    }

    private static func multilineClosingEnd(
        in text: NSString,
        from start: Int,
        to end: Int,
        delimiter: MultilineDelimiter
    ) -> Int? {
        var searchStart = start
        while searchStart < end {
            let found = text.range(
                of: delimiter.marker,
                options: [],
                range: NSRange(location: searchStart, length: end - searchStart)
            )
            guard found.location != NSNotFound else { return nil }
            if delimiter == .literal || !isEscaped(in: text, at: found.location) {
                return NSMaxRange(found)
            }
            searchStart = NSMaxRange(found)
        }
        return nil
    }

    private static func quotedClosingEnd(
        in text: NSString,
        from start: Int,
        to end: Int,
        quote: unichar,
        honorsEscapes: Bool
    ) -> Int {
        var cursor = start
        while cursor < end {
            if text.character(at: cursor) == quote,
               !honorsEscapes || !isEscaped(in: text, at: cursor) {
                return cursor + 1
            }
            cursor += 1
        }
        return end
    }

    private static func isEscaped(in text: NSString, at location: Int) -> Bool {
        var cursor = location
        var slashCount = 0
        while cursor > 0, text.character(at: cursor - 1) == unichar(92) {
            slashCount += 1
            cursor -= 1
        }
        return slashCount.isMultiple(of: 2) == false
    }

    private static func hasMarker(
        _ marker: String,
        in text: NSString,
        at location: Int,
        to end: Int
    ) -> Bool {
        let markerLength = (marker as NSString).length
        guard location + markerLength <= end else { return false }
        return text.substring(with: NSRange(
            location: location,
            length: markerLength
        )) == marker
    }

    private static func isFollowedByAssignment(
        in text: NSString,
        after location: Int,
        to end: Int
    ) -> Bool {
        let next = skipWhitespace(in: text, from: location, to: end)
        return next < end && text.character(at: next) == equals
    }

    private static func isTableHeader(
        in text: NSString,
        from start: Int,
        to end: Int
    ) -> Bool {
        let contentEnd = commentBoundary(in: text, from: start, to: end)
        let last = trimmedEnd(in: text, from: start, to: contentEnd)
        guard last > start else { return false }
        return text.character(at: last - 1) == unichar(93)
    }

    private static func looksLikeNumberOrDate(_ atom: String) -> Bool {
        if atom == "inf" || atom == "+inf" || atom == "-inf" ||
            atom == "nan" || atom == "+nan" || atom == "-nan" {
            return true
        }
        guard let first = atom.utf8.first else { return false }
        if first >= 48 && first <= 57 {
            return true
        }
        if (first == 43 || first == 45),
           let second = atom.utf8.dropFirst().first {
            return second >= 48 && second <= 57
        }
        return false
    }

    private static func isAtomStart(_ character: unichar) -> Bool {
        !isAtomDelimiter(character)
    }

    private static func isAtomDelimiter(_ character: unichar) -> Bool {
        isWhitespace(character) ||
            character == hash ||
            character == unichar(44) ||
            character == equals ||
            character == unichar(91) ||
            character == unichar(93) ||
            character == unichar(123) ||
            character == unichar(125)
    }

    private static func skipWhitespace(
        in text: NSString,
        from start: Int,
        to end: Int
    ) -> Int {
        var cursor = start
        while cursor < end, isWhitespace(text.character(at: cursor)) {
            cursor += 1
        }
        return cursor
    }

    private static func trimmedEnd(
        in text: NSString,
        from start: Int,
        to end: Int
    ) -> Int {
        var cursor = end
        while cursor > start, isWhitespace(text.character(at: cursor - 1)) {
            cursor -= 1
        }
        return cursor
    }

    private static func isWhitespace(_ character: unichar) -> Bool {
        character == unichar(9) ||
            character == unichar(13) ||
            character == unichar(32)
    }

    private static func append(
        _ kind: TOMLSyntaxTokenKind,
        _ range: Range<Int>,
        to result: inout [TOMLSyntaxToken]
    ) {
        guard !range.isEmpty else { return }
        result.append(TOMLSyntaxToken(
            kind: kind,
            range: NSRange(location: range.lowerBound, length: range.count)
        ))
    }
}

struct TOMLPreviewTextView: NSViewRepresentable {
    let contents: String

    func makeNSView(context: Context) -> NSScrollView {
        let scrollView = NSTextView.scrollableTextView()
        scrollView.borderType = .noBorder
        scrollView.drawsBackground = false
        scrollView.hasHorizontalScroller = true
        scrollView.hasVerticalScroller = true
        scrollView.autohidesScrollers = true

        guard let textView = scrollView.documentView as? NSTextView else {
            return scrollView
        }
        textView.isEditable = false
        textView.isSelectable = true
        textView.isRichText = true
        textView.drawsBackground = false
        textView.allowsUndo = false
        textView.usesFindBar = true
        textView.textContainerInset = NSSize(width: 12, height: 12)
        textView.isHorizontallyResizable = true
        textView.isVerticallyResizable = true
        textView.minSize = .zero
        textView.maxSize = NSSize(
            width: CGFloat.greatestFiniteMagnitude,
            height: CGFloat.greatestFiniteMagnitude
        )
        textView.textContainer?.containerSize = NSSize(
            width: CGFloat.greatestFiniteMagnitude,
            height: CGFloat.greatestFiniteMagnitude
        )
        textView.textContainer?.widthTracksTextView = false
        textView.textContainer?.heightTracksTextView = false
        update(textView)
        return scrollView
    }

    func updateNSView(_ scrollView: NSScrollView, context: Context) {
        guard let textView = scrollView.documentView as? NSTextView,
              textView.string != contents else {
            return
        }
        update(textView)
    }

    static func dismantleNSView(
        _ scrollView: NSScrollView,
        coordinator: ()
    ) {
        guard let textView = scrollView.documentView as? NSTextView else {
            return
        }
        textView.textStorage?.setAttributedString(NSAttributedString())
    }

    private func update(_ textView: NSTextView) {
        textView.textStorage?.setAttributedString(
            TOMLSyntaxHighlighter.highlighted(contents)
        )
    }
}
