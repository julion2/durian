"""Platform-independent tests of evidence interpretation, not macOS UI tests."""
import importlib.util
import pathlib
import tempfile
import unittest

spec = importlib.util.spec_from_file_location(
    "diagnosis", pathlib.Path(__file__).with_name("diagnose-keyboard.py"))
diagnosis = importlib.util.module_from_spec(spec)
spec.loader.exec_module(diagnosis)


def event(stage, **details):
    return {"stage": stage, "details": details}


class DiagnosisTests(unittest.TestCase):
    def test_query_copy_does_not_depend_on_ax_or_trace(self):
        for ax in [{"available": False}, {"available": True, "search": False},
                   {"available": True, "search": True, "search_editor_focused": False}]:
            with self.subTest(ax=ax):
                self.assertEqual(diagnosis.classify(True, ax, [], False),
                                 "QUERY_VERIFIED_AX_UNVERIFIED")
        self.assertEqual(diagnosis.classify(True, {
            "available": True, "search": True, "search_editor_focused": True}, [], False),
            "QUERY_VERIFIED")

    def test_search_label_alone_does_not_verify_input(self):
        self.assertEqual(diagnosis.classify(False, {"search": True}, [], False),
                         "SEARCH_OBSERVED_INPUT_UNVERIFIED")
        self.assertEqual(diagnosis.classify(False, {}, [
            event("search.open"), event("search.input", probe_matches=False)], True),
            "SEARCH_OBSERVED_INPUT_UNVERIFIED")

    def test_trace_query_does_not_verify_copy(self):
        self.assertEqual(diagnosis.classify(False, {}, [
            event("search.input", probe_matches=True)], True), "QUERY_RECEIVED_COPY_UNVERIFIED")

    def test_last_observed_boundary_and_unrelated_keys(self):
        native = event("native.key", slash=True, route_to_gpui=True)
        received = event("gpui.received", slash=True)
        cases = [([], "INPUT_UNOBSERVED"),
                 ([event("native.key", slash=False)], "INPUT_UNOBSERVED"),
                 ([native], "NATIVE_RECEIVED_GPUI_UNOBSERVED"),
                 ([native, event("gpui.received", slash=False)], "NATIVE_RECEIVED_GPUI_UNOBSERVED"),
                 ([native, received], "GPUI_RECEIVED_SEARCH_UNOBSERVED"),
                 ([native, received, event("search.open")], "SEARCH_OBSERVED_INPUT_UNVERIFIED")]
        for events, expected in cases:
            with self.subTest(events=events):
                self.assertEqual(diagnosis.classify(False, {}, events, True), expected)
        self.assertEqual(diagnosis.classify(False, {}, [], False), "UNVERIFIED_NO_TRACE")

    def test_log_offset_excludes_earlier_phase_and_partial_line(self):
        with tempfile.TemporaryDirectory(prefix="gpui-trace-test-") as directory:
            path = pathlib.Path(directory) / "native.log"
            before = b'DURIAN_INPUT {"stage":"search.open"}\n'
            after = b'other log\nDURIAN_INPUT {"stage":"native.key"}\nDURIAN_INPUT {"stage":'
            path.write_bytes(before + after)
            self.assertEqual(diagnosis.traces(path, len(before)), [{"stage": "native.key"}])

    def test_html_content_boundaries_are_not_focus_failures(self):
        request = event("html.request", content_exists=True)
        self.assertEqual(diagnosis.html_boundary([event("html.ready")]), "HTML_REQUEST_UNOBSERVED")
        self.assertEqual(diagnosis.html_boundary([event("html.request", content_exists=False)]),
                         "HTML_CONTENT_ABSENT")
        for details, expected in [({"plain": True}, "HTML_CONTENT_NOT_HTML"),
                                  ({"has_html": False}, "HTML_CONTENT_NOT_HTML"),
                                  ({"images_loading": True, "embedded": False}, "HTML_IMAGES_LOADING"),
                                  ({"embedded": False}, "HTML_EMBED_ABSENT")]:
            with self.subTest(details=details):
                self.assertEqual(diagnosis.html_boundary([request, event("html.content", **details)]), expected)

    def test_html_focus_requires_complete_current_evidence(self):
        request = event("html.request", content_exists=True)
        ready = {"native": True, "ready": True, "in_view": True, "hidden": False,
                 "overlay_active": False, "window_present": True, "succeeded": True,
                 "first_responder_in_webkit": True}
        self.assertEqual(diagnosis.html_boundary([request, event("html.focus", **ready)]),
                         "HTML_RESPONDER_VERIFIED")
        for field, expected in [("native", "HTML_NATIVE_ABSENT"),
                                ("ready", "HTML_DOCUMENT_NOT_READY"),
                                ("in_view", "HTML_NOT_VISIBLE"), ("hidden", "HTML_NOT_VISIBLE"),
                                ("overlay_active", "HTML_NOT_VISIBLE"),
                                ("succeeded", "HTML_FOCUS_CALL_FAILED"),
                                ("first_responder_in_webkit", "HTML_RESPONDER_UNVERIFIED")]:
            with self.subTest(field=field):
                changed = {**ready, field: not ready[field]}
                self.assertEqual(diagnosis.html_boundary([request, event("html.focus", **changed)]), expected)
        for missing in ready:
            incomplete = {key: value for key, value in ready.items() if key != missing}
            self.assertEqual(diagnosis.html_boundary([request, event("html.focus", **incomplete)]),
                             "HTML_RESPONDER_UNVERIFIED")
        # A theme rebuild/new request must not inherit the previous view's focus.
        self.assertEqual(diagnosis.html_boundary([request, event("html.focus", **ready), request]),
                         "HTML_FOCUS_UNOBSERVED")


if __name__ == "__main__":
    unittest.main()
