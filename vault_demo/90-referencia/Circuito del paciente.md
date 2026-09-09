---
titulo: Circuito del paciente
estado: vigente
tags:
  - hospital
  - proceso
  - circuito-asistencial
descripcion: Recorrido transversal que encadena las secciones del hospital, desde la entrada hasta el seguimiento.
tipo: proceso
version: "1.0"
autor: Documentación de ejemplo
fecha_creacion: 2026-08-10
derechos: ejemplo-libre
---

# Circuito del paciente

Nota transversal: las secciones del hospital no funcionan por separado, sino encadenadas.
Recoge las vías de entrada, el recorrido tipo de un caso programado y los elementos que
atravesan todo el proceso.

## Objetivo

Mostrar las dependencias entre secciones y situar cada nota del vault dentro del recorrido
real de un paciente.

## Alcance

Aplica al recorrido hospitalario completo, desde la derivación hasta el retorno a atención
primaria. No describe el protocolo clínico de ninguna patología concreta.

## Desarrollo

### Vías de entrada

| Vía | Cómo llega | Primera sección |
|---|---|---|
| Urgente | Por sus medios o en ambulancia | [[Urgencias]] |
| Programada | Derivación desde atención primaria | [[Consultas externas]] |
| Poblacional | Invitación de un programa de cribado | [[Medicina preventiva y salud pública|Medicina preventiva]] |

### Recorrido tipo de un caso programado

1. Derivación: el médico de familia deriva al especialista
2. Cita: [[Admisión y documentación clínica|Admisión]] asigna hueco de agenda
3. Primera consulta: el especialista valora y solicita pruebas
4. Pruebas: [[Laboratorio clínico|laboratorio]], [[Diagnóstico por imagen|imagen]] o
   [[Endoscopia digestiva|endoscopia]] según el caso
5. Diagnóstico: si hubo biopsia, lo confirma [[Anatomia patológica|anatomía patológica]]
6. Decisión terapéutica: alta, tratamiento en [[Hospital de día|hospital de día]], o
   inclusión en lista de espera quirúrgica
7. Intervención: [[Bloque quirúrgico|bloque quirúrgico]], con paso previo por consulta de
   preanestesia
8. Recuperación: reanimación y, si el caso lo requiere, [[Cuidados intensivos]], después
   [[Hospitalización|hospitalización]]
9. Alta: informe de alta, medicación desde [[Farmacia hospitalaria]] y cita de revisión
10. Seguimiento: revisiones en consulta y retorno a atención primaria

### Elementos transversales

- El identificador del paciente: el mismo número de historia en todas las secciones
- La historia clínica electrónica: cada paso deja registro, sostenido por
   [[Sistemas de información|Sistemas de información]]
- Los puntos de espera: entre cada paso hay una demora, y ahí se concentran los indicadores
   de gestión

## Documentos relacionados

- [[Mapa del hospital]]
- [[Glosario hospitalario]]

## Observaciones

> [!info] Modelo simplificado
> Un caso real puede saltar pasos, retroceder o discurrir en paralelo. Este esquema sirve para
> entender las dependencias entre secciones, no para describir un protocolo.

## Control de cambios

| Versión | Fecha | Descripción del cambio | Responsable |
|---|---|---|---|
| 1.0 | 2026-08-10 | Creación inicial | Documentación de ejemplo |