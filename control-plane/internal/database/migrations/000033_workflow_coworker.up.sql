-- HOR-459 (DES-HOR-459-01): workflow presentation becomes a localized title
-- plus the AI coworker identity. The old shape
--   {workflowTitle, personaName, personaAvatar?, locale?}
-- is rewritten to
--   {title:{en,pt}, coworker:{name, mark:{shape,color}, role:{en,pt},
--    summary:{en,pt}, onItsOwn:[], connections:[]}}
-- The old title is used for both locales, the persona name becomes the
-- coworker name, the mark takes the default round/blue, and copy the old shape
-- never had stays empty until the operator publishes a new workflow version.
-- Every presentation that is not already in the new shape (including the
-- empty default) is rewritten, so readers only ever see one shape.

UPDATE workflow.definitions
SET presentation = jsonb_build_object(
        'title', jsonb_build_object(
            'en', COALESCE(presentation->>'workflowTitle', ''),
            'pt', COALESCE(presentation->>'workflowTitle', '')),
        'coworker', jsonb_build_object(
            'name', COALESCE(presentation->>'personaName', ''),
            'mark', '{"shape":"round","color":"blue"}'::jsonb,
            'role', '{"en":"","pt":""}'::jsonb,
            'summary', '{"en":"","pt":""}'::jsonb,
            'onItsOwn', '[]'::jsonb,
            'connections', '[]'::jsonb))
WHERE NOT presentation ? 'coworker';

-- The canonical spec carries the same presentation; keep it in one shape.
UPDATE workflow.definitions
SET spec_json = jsonb_set(spec_json, '{presentation}', presentation)
WHERE spec_json ? 'presentation'
  AND NOT spec_json->'presentation' ? 'coworker';

UPDATE work.attempts
SET presentation_snapshot = jsonb_build_object(
        'title', jsonb_build_object(
            'en', COALESCE(presentation_snapshot->>'workflowTitle', ''),
            'pt', COALESCE(presentation_snapshot->>'workflowTitle', '')),
        'coworker', jsonb_build_object(
            'name', COALESCE(presentation_snapshot->>'personaName', ''),
            'mark', '{"shape":"round","color":"blue"}'::jsonb,
            'role', '{"en":"","pt":""}'::jsonb,
            'summary', '{"en":"","pt":""}'::jsonb,
            'onItsOwn', '[]'::jsonb,
            'connections', '[]'::jsonb))
WHERE NOT presentation_snapshot ? 'coworker';
