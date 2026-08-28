import Foundation
import XCTest
@testable import OpenCodexRelay

final class TOMLSyntaxHighlighterTests: XCTestCase {
    func testHighlightsCoreTOMLTokensWithoutChangingSource() {
        let source = #"""
        # Relay config
        [profiles.default]
        model = "gpt-5#stable" # selected
        enabled = true
        context_window = 272000
        updated_at = 2026-08-24T12:34:56Z
        """#

        XCTAssertEqual(fragments(.table, in: source), ["[profiles.default]"])
        XCTAssertEqual(
            fragments(.key, in: source),
            ["model", "enabled", "context_window", "updated_at"]
        )
        XCTAssertEqual(fragments(.stringLiteral, in: source), ["\"gpt-5#stable\""])
        XCTAssertEqual(fragments(.booleanLiteral, in: source), ["true"])
        XCTAssertEqual(
            fragments(.numberOrDate, in: source),
            ["272000", "2026-08-24T12:34:56Z"]
        )
        XCTAssertEqual(
            fragments(.comment, in: source),
            ["# Relay config", "# selected"]
        )

        let highlighted = TOMLSyntaxHighlighter.highlighted(source)
        XCTAssertEqual(highlighted.string, source)
        XCTAssertGreaterThan(highlighted.length, 0)
    }

    func testMultilineStringDoesNotCreateFalseKeysOrComments() {
        let source = #"""
        prompt = """
        # not a comment
        key = "not an assignment"
        """ # actual comment
        next = false
        """#

        XCTAssertEqual(fragments(.key, in: source), ["prompt", "next"])
        XCTAssertEqual(fragments(.comment, in: source), ["# actual comment"])
        XCTAssertTrue(
            fragments(.stringLiteral, in: source).contains("# not a comment")
        )
        XCTAssertTrue(
            fragments(.stringLiteral, in: source).contains(#"key = "not an assignment""#)
        )
        XCTAssertEqual(fragments(.booleanLiteral, in: source), ["false"])
    }

    func testHighlightsInlineAndQuotedKeysWithoutTreatingHashesAsComments() {
        let source = #"""
        "model#name" = "x#y" # root comment
        tool = { name = "relay", "retry count" = 3 }
        ["a#b"] # table comment
        """#

        XCTAssertEqual(
            fragments(.key, in: source),
            ["\"model#name\"", "tool", "name", "\"retry count\""]
        )
        XCTAssertEqual(
            fragments(.stringLiteral, in: source),
            ["\"x#y\"", "\"relay\""]
        )
        XCTAssertEqual(fragments(.numberOrDate, in: source), ["3"])
        XCTAssertEqual(fragments(.table, in: source), ["[\"a#b\"]"])
        XCTAssertEqual(
            fragments(.comment, in: source),
            ["# root comment", "# table comment"]
        )
    }

    func testArrayContinuationIsNotMistakenForTableHeader() {
        let source = """
        values = [
          [1, 2],
        ]
        """

        XCTAssertTrue(fragments(.table, in: source).isEmpty)
        XCTAssertEqual(fragments(.key, in: source), ["values"])
        XCTAssertEqual(fragments(.numberOrDate, in: source), ["1", "2"])
    }

    func testHighlightingNearPreviewLimitPreservesSource() {
        let line = "model = \"gpt-5\" # active\nenabled = true\n"
        let maximumBytes = 1_048_576
        let source = String(
            repeating: line,
            count: maximumBytes / line.utf8.count
        )

        XCTAssertLessThanOrEqual(source.utf8.count, maximumBytes)
        let highlighted = TOMLSyntaxHighlighter.highlighted(source)
        XCTAssertEqual(highlighted.string, source)
    }

    private func fragments(
        _ kind: TOMLSyntaxTokenKind,
        in source: String
    ) -> [String] {
        let text = source as NSString
        return TOMLSyntaxHighlighter.tokens(in: source)
            .filter { $0.kind == kind }
            .map { text.substring(with: $0.range) }
    }
}
