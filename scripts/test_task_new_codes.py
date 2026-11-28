"""issue #429：新任务码映射与「明确报错」措辞的回归测试。

覆盖：
  - expert_use_* 家族命中 expertlocal（静态专家 id / jump_url 动态 id）
  - skill_share 命中 skillshare，分享渠道请求体与事件字段
  - 原有任务码（create_canvas / chat_5 / expert_5 / expert_use / lighthouse）行为不变
  - 未知码 -> unknown task code；需人工 / 需抓包 -> unsupported task code
  - process_task 对未知码/不支持码立即返回，不发起任何网络请求

运行：cd scripts && python -S -m unittest test_task_new_codes -v
"""
import io
import json
import unittest
from contextlib import redirect_stdout
from types import SimpleNamespace

import task_runner as runner


class ResolveTaskSpecTest(unittest.TestCase):
    def test_known_expert_campaign_codes_map_to_expertlocal(self):
        expected = {
            "expert_use_maidehao": ("ex_GDl31WahyIH0", "卖得好", "1.0.1"),
            "expert_use_douyinyin": ("ex_FbY5nlWTqDod", "斗音音", "1.0.2"),
            "expert_use_shangjia_xuanchuan": ("ex_Weo3x2EZuSDm", "商家宣传设计师", "1.6.0"),
            "expert_use_bowangwang": ("ex_WZY136wV5aMt", "播旺旺", "1.0.2"),
        }
        for code, (eid, name, version) in expected.items():
            with self.subTest(code=code):
                spec = runner.resolve_task_spec(code)
                self.assertEqual(spec["kind"], "expertlocal")
                self.assertEqual(spec["expert"]["id"], eid)
                self.assertEqual(spec["expert"]["name"], name)
                self.assertEqual(spec["expert"]["version"], version)
                self.assertFalse(spec.get("unsupported"))

    def test_unknown_expert_slug_uses_jump_url_expert_id(self):
        task = {"jump_url": "workbuddy://experts?expertId=ex_FbY5nlWTqDod"}
        spec = runner.resolve_task_spec("expert_use_douyinjin", task)
        self.assertEqual(spec["kind"], "expertlocal")
        self.assertEqual(spec["expert"]["id"], "ex_FbY5nlWTqDod")

    def test_unknown_expert_slug_without_id_is_explicit_unsupported(self):
        code = "expert_use_future_slug"
        spec = runner.resolve_task_spec(code)
        self.assertTrue(spec["unsupported"])
        self.assertEqual(spec["reason_kind"], "capture")
        notice = runner.describe_task_code(code, spec)
        self.assertIn("unsupported task code", notice)
        self.assertIn("抓包", notice)

    def test_skill_share_maps_to_skillshare(self):
        spec = runner.resolve_task_spec("skill_share")
        self.assertEqual(spec["kind"], "skillshare")
        self.assertEqual(spec["target"], 1)

    def test_legacy_codes_unchanged(self):
        self.assertEqual(runner.resolve_task_spec("create_canvas")["kind"], "canvas")
        self.assertEqual(runner.resolve_task_spec("chat_5")["kind"], "chat")
        self.assertEqual(runner.resolve_task_spec("chat_5")["target"], 5)
        self.assertEqual(runner.resolve_task_spec("expert_5")["kind"], "expert")
        self.assertEqual(runner.resolve_task_spec("Expert_lighthouse")["kind"], "lighthouse")
        # 开学季的 expert_use 是 school 域，不能被 expert_use_ 前缀家族误吞
        self.assertEqual(runner.resolve_task_spec("expert_use")["kind"], "school")
        self.assertTrue(runner.resolve_task_spec("Expert_Philanthropy")["unforgeable"])

    def test_unknown_code_is_unknown_not_unsupported(self):
        self.assertIsNone(runner.resolve_task_spec("totally_new_task"))
        notice = runner.describe_task_code("totally_new_task", None)
        self.assertIn("unknown task code", notice)
        self.assertIn("不认识", notice)
        self.assertNotIn("unsupported", notice)

    def test_manual_and_capture_codes_are_explicit(self):
        wechat = runner.describe_task_code(
            "wb_wechat_oa_subscribe_task",
            runner.resolve_task_spec("wb_wechat_oa_subscribe_task"))
        self.assertIn("unsupported task code", wechat)
        self.assertIn("需人工", wechat)
        valorant = runner.describe_task_code(
            "WorkBuddy_Valorant", runner.resolve_task_spec("WorkBuddy_Valorant"))
        self.assertIn("unsupported task code", valorant)
        self.assertIn("抓包", valorant)

    def test_missing_known_task_says_not_issued(self):
        notice = runner.describe_missing_task(
            "create_canvas", runner.resolve_task_spec("create_canvas"))
        self.assertIn("服务端未下发", notice)
        self.assertNotIn("unknown task code", notice)


class JumpUrlTest(unittest.TestCase):
    def test_direct_deeplink(self):
        self.assertEqual(
            runner._expert_id_from_jump_url("workbuddy://experts?expertId=ex_GDl31WahyIH0"),
            "ex_GDl31WahyIH0")

    def test_trusted_http_wrapper(self):
        url = ("https://www.workbuddy.cn/launch/?deeplink="
               "workbuddy%3A%2F%2Fexperts%3FexpertId%3Dex_GDl31WahyIH0")
        self.assertEqual(runner._expert_id_from_jump_url(url), "ex_GDl31WahyIH0")

    def test_untrusted_host_rejected(self):
        url = ("https://evil.example/launch/?deeplink="
               "workbuddy%3A%2F%2Fexperts%3FexpertId%3Dex_GDl31WahyIH0")
        self.assertEqual(runner._expert_id_from_jump_url(url), "")

    def test_malformed_expert_id_rejected(self):
        self.assertEqual(
            runner._expert_id_from_jump_url("workbuddy://experts?expertId=ContentCreator"), "")


class SkillShareShapeTest(unittest.TestCase):
    def test_channel_body_uses_json_string_asset_ref(self):
        body = runner.build_skill_share_channel_body(
            {"id": "skill_x", "name": "PPT", "description": "desc"})
        self.assertEqual(body["asset_kind"], "skill")
        self.assertEqual(body["asset_source"], "market")
        self.assertIsInstance(body["asset_ref"], str)
        self.assertEqual(json.loads(body["asset_ref"]), {"asset_id": "skill_x"})
        self.assertEqual(body["ways"], ["link", "wxacode", "wxapath"])
        self.assertEqual(body["extra"]["shareKey"], "skill_x")
        self.assertEqual(body["extra"]["marketplaceSource"], "builtin-market")

    def test_event_shape(self):
        ev = runner.build_skill_share_event({"id": "skill_x", "name": "PPT"}, "ch_1")
        self.assertEqual(ev["eventCode"], "skill_share_channel_action")
        self.assertEqual(ev["action"], "wechat")
        self.assertEqual(ev["panelAction"], "success")
        self.assertEqual(ev["id"], "ch_1")
        self.assertEqual(ev["skillId"], "skill_x")
        self.assertEqual(ev["skillType"], "market")


class IdsForTest(unittest.TestCase):
    def test_expertlocal_returns_fixed_expert(self):
        spec = runner.resolve_task_spec("expert_use_maidehao")
        ids = runner.ids_for(spec, {"uid": "u"}, 1)
        self.assertEqual(ids[0][0], "ex_GDl31WahyIH0")
        self.assertEqual(ids[0][1]["name"], "卖得好")

    def test_expertlocal_unknown_returns_empty(self):
        self.assertEqual(
            runner.ids_for({"kind": "expertlocal", "expert": None}, {"uid": "u"}, 1), [])

    def test_skillshare_returns_placeholder(self):
        self.assertEqual(
            runner.ids_for({"kind": "skillshare"}, {"uid": "u"}, 2),
            [("", {}), ("", {})])

    def test_legacy_kind_string_still_supported(self):
        self.assertEqual(
            runner.ids_for("chat", {"uid": "u"}, 2), [("", {}), ("", {})])


class ProcessTaskMessageTest(unittest.TestCase):
    def _run(self, code):
        stats = {"total": 0, "skip": 0, "pending": 0, "already": 0, "fail": 0, "ok": 0}
        opts = SimpleNamespace(only_claim=False, yes=False, gap=1.0)
        buf = io.StringIO()
        with redirect_stdout(buf):
            runner.process_task(
                {"uid": "u12345678"}, code,
                {"accept_status": "not_accepted",
                 "progress": {"current": 0, "target": 1}},
                opts, stats)
        return buf.getvalue(), stats

    def test_unknown_code_prints_unknown_even_in_process_task(self):
        out, stats = self._run("totally_new_task")
        self.assertIn("unknown task code: totally_new_task", out)
        self.assertEqual(stats["skip"], 1)

    def test_manual_code_prints_unsupported(self):
        out, stats = self._run("wb_wechat_oa_subscribe_task")
        self.assertIn("unsupported task code: wb_wechat_oa_subscribe_task", out)
        self.assertIn("需人工", out)
        self.assertEqual(stats["skip"], 1)


class ProcessAccountDiscoveryTest(unittest.TestCase):
    def test_dynamic_expert_family_is_dispatched_and_others_are_classified(self):
        tasks = [
            {"task_code": "expert_use_douyinjin", "accept_status": "not_accepted",
             "progress": {"current": 0, "target": 1},
             "jump_url": "workbuddy://experts?expertId=ex_FbY5nlWTqDod"},
            {"task_code": "WorkBuddy_Valorant", "accept_status": "not_accepted"},
            {"task_code": "totally_new_task", "accept_status": "not_accepted"},
        ]
        stats = {"accounts": 1, "total": 0, "skip": 0, "pending": 0,
                 "already": 0, "fail": 0, "ok": 0}
        opts = SimpleNamespace(only_codes=None, only_claim=False, yes=False, gap=1.0)
        buf = io.StringIO()
        from unittest import mock
        with mock.patch.object(runner.tc, "do_get", return_value=(-1, {})), \
             mock.patch.object(runner.tc, "list_tasks", return_value=tasks), \
             mock.patch.object(runner, "_mp_task_status", return_value=None), \
             mock.patch.object(runner, "process_minichat_task"), \
             mock.patch.object(runner, "process_school_tasks"), \
             mock.patch.object(runner, "process_task") as process, \
             redirect_stdout(buf):
            runner.process_account({"uid": "u12345678", "nick": "n"}, opts, stats)
        out = buf.getvalue()
        # 动态 expert_use_* 走 process_task（不是只展示）
        self.assertEqual([call.args[1] for call in process.call_args_list],
                         ["expert_use_douyinjin"])
        # 需抓包 / 不认识的任务码各自有明确措辞
        self.assertIn("unsupported task code: WorkBuddy_Valorant", out)
        self.assertIn("unknown task code: totally_new_task", out)
        self.assertNotIn("非映射任务", out)


class LightUpWiringTest(unittest.TestCase):
    def _opts(self):
        return SimpleNamespace(yes=True, gap=1.0, only_claim=False)

    def _stats(self):
        return {"total": 0, "skip": 0, "pending": 0, "already": 0, "fail": 0, "ok": 0}

    def test_skillshare_creates_channel_then_reports_event(self):
        from unittest import mock
        spec = runner.resolve_task_spec("skill_share")
        auth = {"uid": "u12345678", "token": "t"}
        states = [{"accept_status": "accepted"},
                  {"accept_status": "accepted", "progress": {"current": 0, "target": 1}},
                  {"accept_status": "completed", "progress": {"current": 1, "target": 1}}]
        with mock.patch.object(runner.tc, "task_status", side_effect=states), \
             mock.patch.object(runner, "_fetch_share_skill",
                              return_value={"id": "skill_x", "name": "PPT",
                                            "description": "d"}), \
             mock.patch.object(runner, "_create_skill_share_channel",
                              return_value={"ok": True, "channel_id": "ch_1",
                                            "share_code": "sc"}), \
             mock.patch.object(runner, "report_desktop_events",
                              return_value=(200, 0)) as report, \
             mock.patch.object(runner, "claim_one") as claim, \
             mock.patch.object(runner.time, "sleep"):
            runner.light_up(auth, "skill_share", spec, 0, 1, "u1234567",
                            self._opts(), self._stats())
        events = report.call_args_list[0].args[1]
        self.assertEqual(events[0]["eventCode"], "skill_share_channel_action")
        self.assertEqual(events[0]["id"], "ch_1")
        claim.assert_called_once()

    def test_expertlocal_uses_fixed_expert_chain(self):
        from unittest import mock
        spec = runner.resolve_task_spec("expert_use_maidehao")
        auth = {"uid": "u12345678", "token": "t", "nick": "n"}
        rid = "cmb-" + "a1" * 16
        states = [{"accept_status": "accepted"},
                  {"accept_status": "accepted", "progress": {"current": 0, "target": 1}},
                  {"accept_status": "completed", "progress": {"current": 1, "target": 1}}]
        with mock.patch.object(runner.tc, "task_status", side_effect=states), \
             mock.patch.object(runner, "_lighthouse_real_chat",
                              return_value=("conv-1", rid, "")), \
             mock.patch.object(runner, "report_desktop_events",
                              return_value=(200, 0)) as report, \
             mock.patch.object(runner, "claim_one") as claim, \
             mock.patch.object(runner.time, "sleep"):
            runner.light_up(auth, "expert_use_maidehao", spec, 0, 1, "u1234567",
                            self._opts(), self._stats())
        # 第一次上报召唤三连，第二次上报六连 + expert_actual_use
        self.assertEqual(report.call_count, 2)
        chain = report.call_args_list[1].args[1]
        use = [e for e in chain if e.get("eventCode") == "expert_actual_use"][-1]
        self.assertEqual(use["id"], "ex_GDl31WahyIH0")
        self.assertEqual(use["mode"], "LOCAL")
        self.assertEqual(use["requestId"], rid)
        claim.assert_called_once()

    def test_create_skill_share_channel_hits_open_platform(self):
        from unittest import mock
        payload = {"code": 0, "data": {
            "channel_id": "ch_1", "share_code": "sc",
            "ways": [{"share_way": "link", "resource_ref": "https://x"}]}}
        with mock.patch.object(runner.tc, "do_post",
                               return_value=(200, payload)) as post:
            got = runner._create_skill_share_channel(
                {"uid": "u1", "token": "t"}, {"id": "skill_x", "name": "PPT"})
        self.assertTrue(got["ok"])
        self.assertEqual(got["channel_id"], "ch_1")
        args = post.call_args.args
        self.assertEqual(args[1], runner.SKILL_SHARE_BASE)
        self.assertEqual(args[2], runner.SKILL_SHARE_CHANNEL_PATH)
        self.assertEqual(post.call_args.kwargs["headers"]["X-User-Id"], "u1")


if __name__ == "__main__":
    unittest.main()
