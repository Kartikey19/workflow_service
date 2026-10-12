import { useRef, useState } from "react";
import { Plus, Trash2 } from "lucide-react";
import type { RecordData } from "./api";
import { Field } from "./components";

const types = [
  ["string", "Text"],
  ["number", "Number"],
  ["integer", "Integer"],
  ["boolean", "Boolean"],
  ["object", "Object"],
  ["array", "Array"],
  ["any", "Any"],
];

export function WorkflowInputs({
  inputs = {},
  editable,
  onChange,
}: {
  inputs?: RecordData;
  editable: boolean;
  onChange: (inputs: RecordData, group?: string) => void;
}) {
  const rows = Object.entries(inputs);
  const [pendingName, setPendingName] = useState<{
    index: number;
    value: string;
  } | null>(null);
  const [error, setError] = useState<{ index: number; message: string } | null>(
    null,
  );
  const focusNew = useRef<number | null>(null);

  function rename(index: number, name: string) {
    const original = rows[index][0];
    const message = !/^[a-z][a-z0-9_]*$/.test(name)
      ? "Use a lowercase letter first, then letters, numbers or underscores."
      : name !== original && Object.hasOwn(inputs, name)
        ? `Another variable is already called ${name}.`
        : "";
    if (message) {
      setPendingName({ index, value: name });
      setError({ index, message: `${message} The name has not been changed.` });
      return;
    }
    setPendingName(null);
    setError(null);
    // Preserve row order and every schema property, including advanced YAML rules.
    onChange(
      Object.fromEntries(
        rows.map(([key, schema], i) => [i === index ? name : key, schema]),
      ),
      `input:${index}:name`,
    );
  }

  function update(name: string, patch: RecordData) {
    onChange({ ...inputs, [name]: { ...inputs[name], ...patch } });
  }

  function add() {
    let number = 1;
    while (Object.hasOwn(inputs, `variable_${number}`)) number++;
    focusNew.current = rows.length;
    setPendingName(null);
    setError(null);
    onChange({
      ...inputs,
      [`variable_${number}`]: { type: "string", required: true },
    });
  }

  return (
    <div className="workflow-inputs">
      <p className="step-doc">
        Define what each run receives. Steps read these as{" "}
        <code>input.name</code>. Update step references when renaming or
        removing a variable.
      </p>
      {!rows.length && <p className="muted">No input variables defined.</p>}
      {rows.map(([name, raw], index) => {
        const schema = raw || {};
        const type = schema.type || "any";
        const nameError = error?.index === index ? error.message : "";
        return (
          // Index keeps the focused control mounted while its name changes.
          <fieldset className="workflow-input-row" key={index}>
            <legend className="sr-only">Variable {index + 1}</legend>
            <Field label="Name">
              <input
                value={pendingName?.index === index ? pendingName.value : name}
                readOnly={!editable}
                spellCheck={false}
                autoComplete="off"
                aria-invalid={!!nameError}
                aria-errormessage={
                  nameError ? `variable-error-${index}` : undefined
                }
                ref={(field) => {
                  if (field && focusNew.current === index) {
                    focusNew.current = null;
                    field.focus();
                    field.select();
                  }
                }}
                onChange={(e) => rename(index, e.target.value)}
                onBlur={() => setPendingName(null)}
              />
            </Field>
            <Field label="Type">
              <select
                value={type}
                disabled={!editable}
                onChange={(e) => update(name, { type: e.target.value })}
              >
                {!types.some(([value]) => value === type) && (
                  <option value={type}>{type}</option>
                )}
                {types.map(([value, title]) => (
                  <option key={value} value={value}>
                    {title}
                  </option>
                ))}
              </select>
            </Field>
            <label className="input-required">
              <input
                type="checkbox"
                checked={!!schema.required}
                disabled={!editable}
                onChange={(e) => update(name, { required: e.target.checked })}
              />
              Required
            </label>
            {editable && (
              <button
                type="button"
                className="icon-button danger"
                aria-label={`Remove variable ${name}`}
                onClick={() => {
                  setPendingName(null);
                  setError(null);
                  onChange(
                    Object.fromEntries(rows.filter(([key]) => key !== name)),
                  );
                }}
              >
                <Trash2 size={16} aria-hidden="true" />
              </button>
            )}
            <small className="input-reference">
              <code>{`input.${name}`}</code>
            </small>
            {nameError && (
              <p
                className="error input-name-error"
                id={`variable-error-${index}`}
                role="alert"
              >
                {nameError}
              </p>
            )}
          </fieldset>
        );
      })}
      {editable && (
        <button type="button" className="secondary" onClick={add}>
          <Plus size={15} aria-hidden="true" /> Add variable
        </button>
      )}
    </div>
  );
}
