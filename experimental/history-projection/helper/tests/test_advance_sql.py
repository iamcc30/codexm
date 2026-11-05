"""SQL semantics only, not Rust/native acceptance. Schema: pinned Codex 0.166.0."""
import json
from pathlib import Path
import re
import sqlite3
import unittest

HERE = Path(__file__).resolve().parent
SOURCE = (HERE.parent / 'publish_projection.rs').read_text()
TABLES = re.findall(r'table_queries!\("([^"]+)", "([^"]+)"\)', SOURCE)
MIGRATIONS = json.loads((HERE / 'pinned_history_migrations.json').read_text())

def queries(table, columns):
    result = {}
    for name, expression in re.findall(r'(\w+): concat!\((.*?)\),', SOURCE):
        pieces = re.findall(r'"[^"]*"|\$table|\$columns', expression)
        result[name] = ''.join(table if p == '$table' else columns if p == '$columns' else json.loads(p) for p in pieces)
    return result

class AdvanceSql(unittest.TestCase):
    def setUp(self):
        self.db = sqlite3.connect(':memory:', isolation_level=None)
        for name in ('base', 'staged', 'rebuilt'):
            self.db.execute(f"ATTACH ':memory:' AS {name}")
        for name in ('main', 'base', 'staged', 'rebuilt'):
            # Run exact migrations on an independent db, then copy schema objects.
            template = sqlite3.connect(':memory:')
            for migration in MIGRATIONS:
                template.executescript(migration['sql'])
            for kind, obj, sql in template.execute("SELECT type,name,sql FROM sqlite_schema WHERE sql IS NOT NULL ORDER BY CASE type WHEN 'table' THEN 0 ELSE 1 END"):
                sql = sql.replace('CREATE TABLE '+obj, f'CREATE TABLE {name}.{obj}', 1).replace('CREATE UNIQUE INDEX '+obj, f'CREATE UNIQUE INDEX {name}.{obj}', 1).replace('CREATE INDEX '+obj, f'CREATE INDEX {name}.{obj}', 1).replace('CREATE TRIGGER '+obj, f'CREATE TRIGGER {name}.{obj}', 1)
                self.db.execute(sql)
            template.close()
            for thread in ('selected', 'unrelated'):
                self.db.execute(f"INSERT INTO {name}.thread_turns(thread_id,turn_id,rollout_ordinal,status) VALUES(?,?,1,'inProgress')", (thread, 't'))
                self.db.execute(f"INSERT INTO {name}.thread_items(thread_id,turn_id,item_id,rollout_ordinal,created_at_ms,item_json,updated_at_ordinal) VALUES(?,?,?,2,0,'{{}}',2)", (thread, 't', 'i'))
                self.db.execute(f"INSERT INTO {name}.thread_realtime_items VALUES(?,?,3,0,'realtime_session_started','{{}}')", (thread, 'r'))
                self.db.execute(f"INSERT INTO {name}.thread_history_projection_state VALUES(?,100,4)", (thread,))
        for name in ('staged', 'rebuilt'):
            self.db.execute(f"UPDATE {name}.thread_turns SET status='completed' WHERE thread_id='selected'")
            self.db.execute(f"UPDATE {name}.thread_items SET item_json='{{\"new\":true}}',updated_at_ordinal=5 WHERE thread_id='selected'")
            self.db.execute(f"UPDATE {name}.thread_history_projection_state SET next_rollout_byte_offset=200,next_rollout_ordinal=6 WHERE thread_id='selected'")

    def tearDown(self): self.db.close()
    def equal(self, left):
        for table, columns in TABLES:
            q = queries(table, columns)
            for suffix in ('forward', 'reverse'):
                if self.db.execute(q[left+'_'+suffix], ('selected', 'selected')).fetchone()[0]: return False
        return True
    def replace(self):
        self.assertTrue(self.equal('base'))
        self.assertTrue(self.equal('rebuilt'))
        for table, columns in TABLES:
            q = queries(table, columns)
            self.db.execute(q['delete'], ('selected',))
            self.db.execute(q['insert'], ('selected',))
        self.db.execute("UPDATE thread_history_projection_state SET next_rollout_byte_offset=200,next_rollout_ordinal=6 WHERE thread_id='selected'")
    def test_exact_old_and_full_equality(self):
        self.assertTrue(self.equal('base')); self.assertTrue(self.equal('rebuilt'))
    def test_old_payload_mutation_rejected(self):
        self.db.execute("UPDATE main.thread_items SET item_json='bad' WHERE thread_id='selected'")
        self.assertFalse(self.equal('base'))
    def test_old_extra_row_rejected(self):
        self.db.execute("INSERT INTO main.thread_realtime_items VALUES('selected','extra',9,0,'x','{}')")
        self.assertFalse(self.equal('base'))
    def test_old_missing_row_rejected(self):
        self.db.execute("DELETE FROM main.thread_turns WHERE thread_id='selected'")
        self.assertFalse(self.equal('base'))
    def test_staging_payload_mutation_rejected(self):
        self.db.execute("UPDATE staged.thread_turns SET status='bad' WHERE thread_id='selected'")
        self.assertFalse(self.equal('rebuilt'))
    def test_replacement_preserves_unrelated(self):
        before = list(self.db.execute("SELECT * FROM main.thread_items WHERE thread_id='unrelated'"))
        self.db.execute('BEGIN IMMEDIATE'); self.replace(); self.db.execute('COMMIT')
        self.assertEqual(before, list(self.db.execute("SELECT * FROM main.thread_items WHERE thread_id='unrelated'")))
        self.assertEqual('completed', self.db.execute("SELECT status FROM main.thread_turns WHERE thread_id='selected'").fetchone()[0])
    def test_rollback_restores_old_partition(self):
        self.db.execute('BEGIN IMMEDIATE'); self.replace(); self.db.execute('ROLLBACK')
        self.assertTrue(self.equal('base'))
        self.assertEqual((100,4), self.db.execute("SELECT next_rollout_byte_offset,next_rollout_ordinal FROM main.thread_history_projection_state WHERE thread_id='selected'").fetchone())

if __name__ == '__main__': unittest.main()
